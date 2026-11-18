#!/usr/bin/env bash
# jq filters are single-quoted on purpose: their $vars are jq variables.
# shellcheck disable=SC2016
# scenarios.sh <workdir> -- builds one directory per parity case under <workdir>/<case>/ and
# prints "<case><TAB><args>" per line. The engine runs with <workdir>/<case> as its working
# directory and <args> split on spaces.
#
# The SAME builder fed both engines: the retired Python checker (CPython 3.9.6, the
# /usr/bin/python3 the pre-push hook ran) wrote the goldens in golden/, and main_test.go replays
# every case against the Go port and requires byte-identical stdout, stderr, exit code and, for
# --sync-edges cases, the rewritten sync.json.
#
# What the cases exercise, by the Python semantics the port had to reproduce:
#   * json.loads: every CPython _json scanner error message and its line/column/char position
#     (code points, not bytes), NaN/Infinity, lone and paired \u surrogates, duplicate keys,
#     big ints, -0.0, float repr, the UTF-8 BOM refusal;
#   * open(): strict UTF-8 and universal newlines (CRLF and lone CR become LF before anything
#     else sees the text);
#   * str.splitlines() boundaries (\v \f \x1c-\x1e \x85 U+2028 U+2029) and str.strip()'s
#     Unicode whitespace in the ledger;
#   * re: Unicode \b, \s and \d, re.I folding (U+0130, U+0131, U+017F, U+212A), the negative
#     lookahead and lookbehind, greedy backtracking in the credential patterns;
#   * %r: repr() quote choice and escapes (\x, \u, \U, non-printables), floats, big ints, nested
#     lists and dicts; %s of non-strings; stderr's backslashreplace of lone surrogates;
#   * json.dumps (sort_keys, separators, ensure_ascii=False; indent=2 for --sync-edges);
#   * Python dynamic typing: str iterated as characters, `in` on a str is a substring test,
#     unhashable values in set/dict membership, None/str ordering. A case where the Python checker died
#     with a traceback is a case where the port must die too (exit 1, "decisions: crash: <Type>").
#
# Credential-shaped values are assembled at run time so this file never carries one.
# Needs bash, jq and perl. Bash 3.2 safe.
set -euo pipefail
W="$1"
mkdir -p "$W"
export LC_ALL=C

rep() { local s="" i; for ((i = 0; i < $2; i++)); do s="$s$1"; done; printf '%s' "$s"; }
emit() { printf '%s\t%s\n' "$1" "$2"; }
fresh() { rm -rf "${W:?}/$1"; mkdir -p "$W/$1"; }

# ---------------------------------------------------------------- the small base document ---
BASE="$W/.base.json"
cat >"$BASE" <<'EOF'
{"generated_at":"2026-01-03T00:00:00Z","topics":{"people":"who may use it","ui":"the screens","ops":"running it"},
"nodes":[
{"id":"D-handles","title":"Handles","decision":"Players pick a handle.","why":"Names are private.","kind":"directive","source":{"session":"aaaaaaaa-0000-4000-8000-000000000001","timestamp":"2026-01-01T00:00:00Z","quote":"use handles","ref":"aaaaaaaa:L10"},"decided_by":"operator","status":"active","depends_on":[],"origin":"operator-turn","topics":["people"]},
{"id":"D-kick","title":"Kick","decision":"Admins may kick.","why":"Abuse happens.","kind":"directive","source":{"session":"aaaaaaaa-0000-4000-8000-000000000001","timestamp":"2026-01-02T00:00:00Z","quote":"let me kick people","ref":"aaaaaaaa:L20"},"decided_by":"operator","status":"active","depends_on":["D-handles"],"coexists_with":["D-handles"],"origin":"operator-turn","topics":["people"]},
{"id":"D-dark","title":"Dark mode","decision":"The UI follows the system theme.","why":"Night use.","kind":"feature","source":{"session":"bbbbbbbb-0000-4000-8000-000000000002","timestamp":"2026-01-02T12:00:00Z","quote":"assistant inferred"},"decided_by":"assistant-best-guess","best_guess_reason":"Nobody said otherwise.","unknown":true,"status":"active","depends_on":[],"origin":"assistant","topics":["ui"]},
{"id":"D-old-deploy","title":"Deploy by hand","decision":"Deploys are manual.","why":"Early days.","kind":"policy","source":{"session":"bbbbbbbb-0000-4000-8000-000000000002","timestamp":"2026-01-01T06:00:00Z","quote":"deploy it yourself","ref":"bbbbbbbb:L5"},"decided_by":"operator","status":"superseded","superseded_by":"D-auto-deploy","depends_on":[],"origin":"operator-turn","topics":["ops"]},
{"id":"D-auto-deploy","title":"Deploy on land","decision":"Every land deploys.","why":"Less toil.","kind":"policy","source":{"session":"bbbbbbbb-0000-4000-8000-000000000002","timestamp":"2026-01-02T06:00:00Z","quote":"deploy on every land","ref":"bbbbbbbb:L9","refs":["bbbbbbbb:L11"]},"decided_by":"operator","status":"active","depends_on":[],"origin":"pasted-content","ratified_by":"bbbbbbbb:L11","topics":["ops"]}
],
"ingest":{"cursor":{"aaaaaaaa":30,"bbbbbbbb":12},"triaged":[{"ref":"aaaaaaaa:L25","reason":"a status ping"},{"ref":"bbbbbbbb:L7","reason":"a question","covered_by":"D-auto-deploy"}]},
"edges":[{"from":"D-kick","to":"D-handles","type":"depends_on"},{"from":"D-kick","to":"D-handles","type":"coexists_with"},{"from":"D-old-deploy","to":"D-auto-deploy","type":"superseded_by"}]}
EOF
LEDGER="$W/.ledger.tsv"
{
	printf '# base ledger\n'
	printf 'D-handles\t560839ff36b8\tadded\n'
	printf 'D-kick\tcd0313e9c78c\tadded\n'
	printf 'D-dark\t875f5d3af649\tadded\n'
	printf 'D-old-deploy\t178e57d1a26a\tadded\n'
	printf 'D-auto-deploy\t632743645793\tadded\n'
} >"$LEDGER"

# Raw JSON tokens jq cannot write are spliced in after jq: a string "@@NAME@@" (quotes included)
# becomes the token below.
splice() {
	perl -CSD -pe '
		my %t = (
			NAN => "NaN", INF => "Infinity", NINF => "-Infinity", NEG0 => "-0.0", NEGZI => "-0",
			E16 => "1e16", E15 => "1e15", E400 => "1e400", EM5 => "1e-05", EM4 => "0.0001", F15 => "1.5e-7",
			BIG => "123456789012345678901234567890123456789", FLT => "2.50", ONE => "1.0",
			SURR => "\"\\ud800\"", SURRLO => "\"\\udc00x\"", PAIR => "\"\\ud83d\\ude00\"",
			ESC => "\"\\u0000\\u001f\\u007f\\b\\f\\n\\r\\t\\\\\\\"\\/\"",
		);
		s/"\@\@([A-Z0-9]+)\@\@"/exists $t{$1} ? $t{$1} : die "unknown token $1"/ge;
	'
}

# mut <case> <jq filter> [jq args...]: the base document through a jq filter, base ledger.
mut() {
	local name="$1" filter="$2"
	shift 2
	fresh "$name"
	jq "$@" "$filter" "$BASE" | splice >"$W/$name/d.json"
	cp "$LEDGER" "$W/$name/ledger.tsv"
	emit "$name" "d.json --ledger ledger.tsv"
}
# raw <case> [args]: d.json from stdin, base ledger.
raw() {
	local name="$1" args="${2:-d.json --ledger ledger.tsv}"
	fresh "$name"
	cat >"$W/$name/d.json"
	cp "$LEDGER" "$W/$name/ledger.tsv"
	emit "$name" "$args"
}
# led <case>: the base document with the ledger from stdin.
led() {
	local name="$1"
	fresh "$name"
	cp "$BASE" "$W/$name/d.json"
	cat >"$W/$name/ledger.tsv"
	emit "$name" "d.json --ledger ledger.tsv"
}
# -------------------------------------------------------------------------- base and modes ---
mut base-ok '.'
mut base-fingerprint '.'
printf 'base-fingerprint-mode\t--fingerprint d.json\n'
mkdir -p "$W/base-fingerprint-mode" && cp "$W/base-fingerprint/d.json" "$W/base-fingerprint-mode/"
fresh sync-append
jq 'del(.edges)' "$BASE" >"$W/sync-append/sync.json"
emit sync-append "--sync-edges sync.json"
fresh sync-replace
jq '.edges = [] | .nodes[0].superseded_in_part_by = ["D-kick"] | .nodes[0].refines = ["D-dark","D-kick"] | .nodes += [.nodes[1]]' "$BASE" >"$W/sync-replace/sync.json"
emit sync-replace "--sync-edges sync.json"
fresh sync-values
jq '.nodes[0].why = {"z":[1,"@@FLT@@",{"b":null,"a":true}],"a":[],"e":{},"n":"@@NAN@@","i":"@@INF@@","ni":"@@NINF@@","z0":"@@NEG0@@","e16":"@@E16@@","e15":"@@E15@@","em5":"@@EM5@@","em4":"@@EM4@@","f15":"@@F15@@","big":"@@BIG@@","one":"@@ONE@@","u":"caf\u00e9 \u2028 \u0085 \ud83d\ude00","esc":"@@ESC@@","pair":"@@PAIR@@"} | .nodes[1].title = "\u00e9t\u00e9 \u65e5\u672c"' "$BASE" | splice >"$W/sync-values/sync.json"
emit sync-values "--sync-edges sync.json"
fresh sync-missing-id
jq '.nodes[2] |= del(.id)' "$BASE" >"$W/sync-missing-id/sync.json"
emit sync-missing-id "--sync-edges sync.json"
fresh sync-surrogate
jq '.nodes[0].why = "@@SURR@@"' "$BASE" | splice >"$W/sync-surrogate/sync.json"
emit sync-surrogate "--sync-edges sync.json"

mut fp-values '.nodes[0].title = {"z":[1,"@@FLT@@",{"b":null,"a":true}],"a":"@@NAN@@","n":"@@BIG@@","e":"@@E16@@","u":"\u00e9\u2028\u007f\ud83d\ude00","c":"@@ESC@@","neg":"@@NEG0@@"} | .nodes[1].source.extra = "ignored" | .nodes[2].source = "not an object" | .nodes[3].decision = ["a", 2, "@@ONE@@"]'
mkdir -p "$W/fp-values-mode" && cp "$W/fp-values/d.json" "$W/fp-values-mode/"
printf 'fp-values-mode\t--fingerprint d.json\n'
fresh fp-not-dict
jq '.nodes += ["just a string"]' "$BASE" >"$W/fp-not-dict/d.json"
emit fp-not-dict "--fingerprint d.json"
fresh fp-no-id
jq '.nodes[1] |= del(.id)' "$BASE" >"$W/fp-no-id/d.json"
emit fp-no-id "--fingerprint d.json"
fresh fp-surrogate
jq '.nodes[0].title = "@@SURR@@"' "$BASE" | splice >"$W/fp-surrogate/d.json"
emit fp-surrogate "--fingerprint d.json"
fresh fp-invalid-json
printf '{"nodes": [' >"$W/fp-invalid-json/d.json"
emit fp-invalid-json "--fingerprint d.json"

# ----------------------------------------------------------------------------- arguments ---
fresh args-ledger-no-value
cp "$BASE" "$W/args-ledger-no-value/d.json"
emit args-ledger-no-value "d.json --ledger"
fresh args-missing-file
emit args-missing-file "no-such.json --ledger ledger.tsv"
fresh args-dir
mkdir "$W/args-dir/adir"
emit args-dir "adir"
fresh args-sync-not-first
cp "$BASE" "$W/args-sync-not-first/d.json"
cp "$LEDGER" "$W/args-sync-not-first/ledger.tsv"
emit args-sync-not-first "d.json --sync-edges --ledger ledger.tsv"
fresh args-two-ledgers
cp "$BASE" "$W/args-two-ledgers/d.json"
cp "$LEDGER" "$W/args-two-ledgers/ledger.tsv"
emit args-two-ledgers "--ledger ledger.tsv --ledger nope.tsv d.json"

# ------------------------------------------------------------------- encoding and newlines ---
printf '' | raw enc-empty
printf ' \n\t ' | raw enc-whitespace
{ printf '\xef\xbb\xbf'; cat "$BASE"; } | raw enc-bom
{ cat "$BASE"; printf '\xff'; } | raw enc-invalid-utf8
perl -pe 's/\n/\r/' "$BASE" | raw enc-cr-only
perl -pe 's/\n/\r\n/' "$BASE" | raw enc-crlf
fresh enc-ledger-invalid-utf8
cp "$BASE" "$W/enc-ledger-invalid-utf8/d.json"
{ cat "$LEDGER"; printf '# \xc3\x28\n'; } >"$W/enc-ledger-invalid-utf8/ledger.tsv"
emit enc-ledger-invalid-utf8 "d.json --ledger ledger.tsv"
# A raw CR inside a JSON string is an LF by the time json.loads sees it.
printf '{"a": "x\ry"}' | raw enc-cr-in-string
printf '{"a":\r\n\r\n  "x\ty"}' | raw enc-crlf-position
printf '["\xc3\xa9\xe6\x97\xa5\xf0\x9f\x98\x80", tru]' | raw enc-multibyte-position

# ---------------------------------------------------------------------------- JSON errors ---
n=0
while IFS= read -r doc; do
	n=$((n + 1))
	printf '%b' "$doc" | raw "json-$(printf '%02d' "$n")"
done <<'EOF'
{
[
[1,]
{"a":1,}
{"a" 1}
{"a":}
{1:2}
[1 2]
"abc
"a\\x"
"a\\u12"
"a\\u12zz"
"\x01"
tru
-
-a
01
1.
1.e5
1e
[1]x
{"a":1}  x
nul
[-Inf]
{"a":[1,2
[,]
{,}
{"a":1 "b":2}
"a\\
"a\\u
{"a"
{"a":1
[1,
[\n\n  ,
"\xc3\xa9\x02"
\xc3\xa9
{}\x0b
{}\x0c
"\\ud800\\u12zz"
"\\ud800\\udc0"
"\\ud800\\x"
"\\ud800\\"
"\\ud800\\udc00
"\\ud800\\udc00x
"\\ud800\\x0000"
"\\ud800\\\\u0041"
"\\uDEAD\\uBEEF\\uZZZZ"
[1E, 2]
[1e+, 2]
[1.5e-, 2]
[-0.5e+3x]
[NaN, Infinity, -Infinity, -Infinit]
[null, true, false, nulL]
{"nodes": "x"}
42
[]
"\\u00e9\\uD83D\\uDE00\\ud800"
{"nodes": [], "generated_at": 0, "topics": []}
EOF

# ----------------------------------------------------------------------- top-level shape ---
mut shape-no-generated 'del(.generated_at)'
mut shape-generated-empty '.generated_at = ""'
mut shape-no-topics 'del(.topics)'
mut shape-topics-empty '.topics = {}'
mut shape-topics-list '.topics = ["people","ui","ops"]'
mut shape-nodes-object '.nodes = {"a": 1}'
mut shape-node-not-object '.nodes += ["a string", 7, null, ["D-x"]]'
mut shape-ok-conflicts '.nodes[1].status = "conflict-open" | .nodes[1].conflicts_with = ["D-handles"] | .nodes[1].conflict = {"question":"which?","best_guess":"both"} | .edges += [{"from":"D-kick","to":"D-handles","type":"conflicts_with"}]'

# ----------------------------------------------------------------------------------- ids ---
mut id-missing '.nodes[0] |= del(.id)'
mut id-values '.nodes[0].id = 7 | .nodes[1].id = "d-lower" | .nodes[2].id = "D-" | .nodes[3].id = "D-A" | .nodes[4].id = "D-a_b"'
mut id-more-values '.nodes[0].id = null | .nodes[1].id = "D-\u00e9t\u00e9" | .nodes[2].id = "D-x\n" | .nodes[3].id = "" | .nodes[4].id = true'
mut id-list '.nodes[0].id = ["D-handles"]'
mut id-dict '.nodes[0].id = {"D-handles": 1}'
mut id-float '.nodes[0].id = "@@ONE@@" | .nodes[1].id = 1 | .nodes[2].id = true'
mut id-dup '.nodes += [.nodes[0], .nodes[2]]'
mut id-dup-diff '.nodes += [.nodes[0] | .title = "Other"]'

# -------------------------------------------------------------------------------- fields ---
mut fields-all-missing '.nodes[0] = {"id": "D-handles"}'
mut fields-some-missing '.nodes[2] |= del(.why, .kind, .status, .origin)'
mut repr-kinds '.nodes[0].kind = "it'"'"'s" | .nodes[1].kind = "say \"hi\" it'"'"'s" | .nodes[2].kind = "tab\tnl\ncr\rbs\\ del\u007f nul\u0000" | .nodes[3].kind = "\u200b\u00a0\u00ad\u2028\u3000\u00e9\ud83d\ude00\ue000" | .nodes[4].kind = "@@SURR@@"'
mut repr-statuses '.nodes[0].status = "@@E16@@" | .nodes[1].status = "@@NAN@@" | .nodes[2].status = "@@NINF@@" | .nodes[3].status = "@@BIG@@" | .nodes[4].status = "@@NEG0@@"'
mut repr-deciders '.nodes[0].decided_by = "@@EM5@@" | .nodes[1].decided_by = "@@EM4@@" | .nodes[2].decided_by = "@@F15@@" | .nodes[3].decided_by = false | .nodes[4].decided_by = "@@FLT@@"'
mut repr-origins '.nodes[0].origin = "@@NEGZI@@" | .nodes[1].origin = null | .nodes[2].origin = "Operator-Turn" | .nodes[3].origin = "@@E400@@" | .nodes[4].origin = "@@ONE@@"'
mut kind-list '.nodes[2].kind = ["feature"]'
mut status-dict '.nodes[2].status = {"a": 1}'
mut decider-list '.nodes[2].decided_by = []'
mut origin-dict '.nodes[2].origin = {}'

# -------------------------------------------------------------------------------- source ---
mut source-string '.nodes[2].source = "from chat"'
mut source-string-no-ingest '.nodes[2].source = "from chat" | del(.ingest)'
mut source-list-falsy '.nodes[2].source = [] | .nodes[3].source = 0 | .nodes[4].source = null'
mut source-empty-fields '.nodes[0].source = {"session": "", "timestamp": "", "quote": ""} | .nodes[1].source = {} | .nodes[2].source.quote = 0'
mut source-quote-null '.nodes[2].source.quote = null'
mut source-quote-int '.nodes[2].source.quote = 5'
mut source-quote-list '.nodes[2].source.quote = [1,2,3]'
mut source-quote-long '.nodes[0].source.quote = $a | .nodes[1].source.quote = $b | .nodes[2].source.quote = $c' --arg a "$(rep x 201)" --arg b "$(rep $'\xf0\x9f\x98\x80' 200)" --arg c "$(rep $'\xc3\xa9' 201)"
mut source-ts-bad '.nodes[0].source.timestamp = "2026-01-01 00:00:00" | .nodes[1].source.timestamp = "2026-01-02T00:00:00Z " | .nodes[2].source.timestamp = "2026-01-02T12:00:00+00:00"'
mut source-ts-unicode-digits '.nodes[0].source.timestamp = "\u0662\u0660\u0662\u0666-01-01T00:00:00Z" | .nodes[1].source.timestamp = "\uff12\uff10\uff12\uff16-01-02T00:00:00Z" | .nodes[2].source.timestamp = "\u00b2026-01-02T12:00:00Z"'
mut source-ts-int '.nodes[2].source.timestamp = 20260101'
mut source-ts-falsy '.nodes[0].source.timestamp = 0 | .nodes[3].source.timestamp = null'
mut source-refs '.nodes[0].source.ref = "aaaaaaaa:L" | .nodes[1].source.ref = "AAAAAAAA:L20" | .nodes[2].source.ref = "bbbbbbbb:L1" | .nodes[2].source.refs = ["bbbbbbbb:L2", "nope", 7, "@@ONE@@", true] | .nodes[3].source.ref = "cccccccc:L5" | .nodes[4].source.refs = "bbbbbbbb:L11"'
mut source-refs-int '.nodes[2].source.refs = 7'
mut source-refs-dict '.nodes[2].source.refs = {"bbbbbbbb:L3": 1, "x": 2}'
mut source-refs-list-item '.nodes[2].source.refs = [["bbbbbbbb:L3"]]'
mut source-ref-session '.nodes[0].source.session = 7 | .nodes[1].source |= del(.session)'

# --------------------------------------------------------------------- lists and unknowns ---
mut lists-shape '.nodes[0].depends_on = "D-kick" | .nodes[1].refines = [] | .nodes[2].implements = "D-kick" | .nodes[3].conflicts_with = {} | .nodes[4].coexists_with = null'
mut lists-missing-dep '.nodes[0] |= del(.depends_on)'
mut unknowns '.nodes[0].unknown = true | .nodes[1].unknown = 1 | .nodes[1].best_guess_reason = "r" | .nodes[2] |= del(.best_guess_reason) | .nodes[3].unknown = "" | .nodes[4].decided_by = "assistant-best-guess"'

# ---------------------------------------------------------------------- origin and ratify ---
mut origin-rules '.nodes[0].origin = "operator-answer" | .nodes[0].source |= del(.ref) | .nodes[1].origin = "relayed" | .nodes[1].decided_by = "assistant-best-guess" | .nodes[2].origin = "assistant" | .nodes[2].decided_by = "operator" | .nodes[3].origin = "machine-hook" | .nodes[4] |= del(.ratified_by)'
mut ratify-rules '.nodes[0].ratified_by = "nope" | .nodes[1].ratified_by = "aaaaaaaa:L99" | .nodes[2].ratified_by = 7 | .nodes[3].ratified_by = "" | .nodes[4].ratified_by = "bbbbbbbb:L1"'
mut ratify-substring '.nodes[4].source.refs = "x bbbbbbbb:L11 y"'
mut ratify-dict '.nodes[4].source.refs = {"bbbbbbbb:L11": true}'

# -------------------------------------------------------------------------------- topics ---
mut topics-shape '.nodes[0].topics = "people" | .nodes[1].topics = [] | .nodes[2] |= del(.topics) | .nodes[3].topics = ["ops", "nope", 7, null] | .nodes[4].topics = {"ops": 1}'
mut topics-list-item '.nodes[0].topics = [["people"]]'
mut topics-int '.nodes[0].topics = 5'
mut topics-unused '.topics.zeta = "none" | .topics.alpha = "none" | .topics["\u00e9"] = "none"'
mut topics-unrelated '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with"))'
mut topics-unrelated-ts '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with")) | .nodes[0].source.timestamp = "2026-01-05T00:00:00Z" | .nodes += [(.nodes[1] | .id = "D-ban" | .source.ref = "aaaaaaaa:L21" | .depends_on = [] | .source.timestamp = "")]'
mut topics-ts-none-mixed '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with")) | .nodes[0].source.timestamp = null'
mut topics-ts-none-both '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with")) | .nodes[0].source.timestamp = null | .nodes[1].source.timestamp = null'
mut topics-ts-zero-false '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with")) | .nodes[0].source.timestamp = 0 | .nodes[1].source.timestamp = false'
mut topics-ts-list-dict '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with")) | .nodes[0].source.timestamp = [] | .nodes[1].source.timestamp = {}'
mut topics-ts-dict-both '.nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with")) | .nodes[0].source.timestamp = {} | .nodes[1].source.timestamp = {}'
mut topics-surrogate '.topics["@@SURR@@"] = "x" | .nodes[0].topics += ["@@SURR@@"] | .nodes[1].topics += ["@@SURR@@"] | .nodes[1] |= del(.coexists_with) | .edges |= map(select(.type != "coexists_with"))'
mut topics-kinds '.nodes[2].kind = "policy" | .nodes[2].topics = ["people"] | .nodes[1].status = "partially-superseded"'

# ------------------------------------------------------------------- status against edges ---
mut status-rules '.nodes[0].status = "superseded" | .nodes[1].superseded_by = "D-dark" | .nodes[2].status = "partially-superseded" | .nodes[3].superseded_in_part_by = ["D-kick"] | .nodes[4].status = "conflict-open"'
mut status-more '.nodes[0].superseded_in_part_by = ["D-kick"] | .nodes[1].conflict = {"question": "q"} | .nodes[2].conflicts_with = ["D-kick"] | .nodes[3].status = "conflict-open" | .nodes[3].conflict = "nope" | .nodes[4].status = "partially-superseded" | .nodes[4].superseded_in_part_by = ["D-dark"] | .nodes[4].remaining = "the rest"'
mut status-superseded-inpart '.nodes[3].superseded_in_part_by = ["D-kick"]'

# ------------------------------------------------------------------------ edges and time ---
mut refs-self '.nodes[0].depends_on = ["D-handles"] | .nodes[2].refines = ["D-dark", "D-nope"] | .nodes[2].implements = ["D-kick", "D-gone"]'
mut refs-self-sup '.nodes[2].superseded_by = "D-dark" | .nodes[2].status = "superseded"'
mut refs-sup-missing '.nodes[3].superseded_by = "D-nope"'
mut refs-sup-list '.nodes[3].superseded_by = ["D-auto-deploy"]'
mut refs-sup-empty-list '.nodes[2].superseded_by = []'
mut refs-string-iter '.nodes[2].refines = "D-kick" | .nodes[2].implements = {"D-kick": 1, "D-zz": 2}'
mut refs-int-iter '.nodes[2].refines = 5'
mut refs-true-iter '.nodes[2].refines = true'
mut refs-list-item '.nodes[2].refines = [["D-kick"]]'
mut refs-surrogate '.nodes[2].refines = ["@@SURR@@"]'
mut time-older '.nodes[3].source.timestamp = "2026-01-03T00:00:00Z"'
mut time-inpart-older '.nodes[3].status = "partially-superseded" | .nodes[3] |= del(.superseded_by) | .nodes[3].superseded_in_part_by = ["D-auto-deploy", "D-nope"] | .nodes[3].remaining = "r" | .nodes[3].source.timestamp = "2026-01-09T00:00:00Z"'
mut time-refines-newer '.nodes[0].refines = ["D-kick"]'
mut time-refines-empty-ts '.nodes[0].refines = ["D-kick"] | .nodes[1].source.timestamp = ""'
mut time-ts-none-compare '.nodes[3].source.timestamp = null | .nodes[4].source.timestamp = null'
mut time-ts-none-str '.nodes[3].source.timestamp = null'
mut time-ts-zero-list '.nodes[3].source.timestamp = 0 | .nodes[4].source.timestamp = []'
mut time-ts-list-list '.nodes[3].source.timestamp = [] | .nodes[4].source.timestamp = []'
mut time-conflict-superseded '.nodes[1].status = "conflict-open" | .nodes[1].conflicts_with = ["D-old-deploy"] | .nodes[1].conflict = {"question": "q", "best_guess": "g"}'
mut cycle-two '.nodes[0].depends_on = ["D-kick"]'
mut cycle-three '.nodes[0].implements = ["D-dark"] | .nodes[2].refines = ["D-kick"]'
mut cycle-via-sup '.nodes[4].depends_on = ["D-old-deploy"]'

# --------------------------------------------------------------------------------- ingest ---
mut ingest-missing 'del(.ingest)'
mut ingest-shapes '.ingest = {"cursor": [], "triaged": []}'
mut ingest-triaged-dict '.ingest.triaged = {}'
mut ingest-cursor '.ingest.cursor = {"aaaaaaaa": 30, "bbbbbbbb": true, "cccccccc": 0, "dddddddd": "@@ONE@@", "EEEEEEEE": 3, "ffff": 3, "99999999": -1, "aaaaaaab": "7"}'
mut ingest-triaged '.ingest.triaged = ["aaaaaaaa:L1", {"ref": "aaaaaaaa:L2"}, {"reason": "x"}, {"ref": "zz", "reason": "x"}, {"ref": "aaaaaaaa:L25", "reason": "a"}, {"ref": "aaaaaaaa:L25", "reason": "b"}, {"ref": "aaaaaaaa:L10", "reason": "cited"}, {"ref": "bbbbbbbb:L11", "reason": "cited via refs"}, {"ref": "aaaaaaaa:L26", "reason": "x", "covered_by": "D-nope"}, {"ref": "aaaaaaaa:L31", "reason": "past"}, {"ref": "cccccccc:L1", "reason": "no cursor"}, {"ref": "aaaaaaaa:L0030", "reason": "padded"}, null, 3]'
mut ingest-triaged-int-ref '.ingest.triaged = [{"ref": 7, "reason": "x"}]'
mut ingest-cursor-string-compare '.ingest.cursor.aaaaaaaa = "30"'
mut ingest-cursor-float-compare '.ingest.cursor.aaaaaaaa = "@@FLT@@" | .ingest.triaged += [{"ref": "aaaaaaaa:L2", "reason": "x"}, {"ref": "aaaaaaaa:L3", "reason": "y"}]'
mut ingest-cursor-big '.ingest.cursor.aaaaaaaa = "@@BIG@@" | .ingest.triaged += [{"ref": "aaaaaaaa:L99999999999999999999999999999999999999999", "reason": "x"}]'
mut ingest-covered-list '.ingest.triaged[1].covered_by = ["D-auto-deploy"]'
mut ingest-covered-falsy '.ingest.triaged[1].covered_by = []'
mut ingest-cited-unhashable '.nodes[2].source.ref = ["x"]'
mut ingest-refs-chars '.nodes[2].source.refs = "ab"'

# ---------------------------------------------------------------------------------- edges ---
mut edges-missing 'del(.edges)'
mut edges-string '.edges = "abc"'
mut edges-null '.edges = null'
mut edges-int '.edges = 3'
mut edges-dict '.edges = {"from": 1}'
mut edges-extra '.edges += [{"from":"D-dark","to":"D-kick","type":"refines"}]'
mut edges-non-dict '.edges += ["x", 1, null, []]'
mut edges-extra-keys '.edges[0].weight = 2'
mut edges-unhashable '.edges[0].to = ["D-handles"]'
mut edges-int-field '.edges[0].to = 1'

# --------------------------------------------------------------------------------- ledger ---
led ledger-format <<EOF
# comment
${LEDGER_PAD:-}

D-handles	560839ff36b8
D-handles	560839ff36b8	added	extra
d-kick	cd0313e9c78c	added
D-kick	CD0313E9C78C	added
D-kick	cd0313e9c78	added
D-kick	cd0313e9c78c	added
D-dark	875f5d3af649	added
D-dark	875f5d3af649	again
D-old-deploy	000000000000	added
D-auto-deploy	632743645793	added
D-gone	123456789abc	added
 # not a comment
EOF
printf '# x\r\nD-handles\t560839ff36b8\tadded\r\nD-kick\tcd0313e9c78c\tadded\rD-dark\t875f5d3af649\tadded\x0bD-old-deploy\t178e57d1a26a\tadded\x0cD-auto-deploy\t632743645793\tadded\x1cD-x\t000000000000\tadded\x1dD-y\t000000000000\tadded\x1eD-z\t000000000000\tadded\xc2\x85D-w\t000000000000\tadded\xe2\x80\xa8D-v\t000000000000\tadded\xe2\x80\xa9D-u\t000000000000\tadded\x1f\tkeep\n\xe3\x80\x80\xc2\xa0\x1f\n' | led ledger-splitlines
printf 'D-handles\t560839ff36b8\tadded\nD-kick\tcd0313e9c78c\tadded\nD-dark\t875f5d3af649\tadded\nD-old-deploy\t178e57d1a26a\tadded\nD-auto-deploy\t632743645793\tadded\nD-kick\tcd0313e9c78c\tcorrected:\nD-kick\tcd0313e9c78c\t corrected: no\nD-kick\tcd0313e9c78c\tCorrected: no\n' | led ledger-corrected
printf '' | led ledger-empty
fresh ledger-bad-ids
jq '.nodes[0].id = 7 | .nodes[1].id = "bad id" | .nodes[2].id = null | .nodes[3].id = true | .nodes[4].id = "@@ONE@@" | .nodes += [{"id": 1}]' "$BASE" | splice >"$W/ledger-bad-ids/d.json"
cp "$LEDGER" "$W/ledger-bad-ids/ledger.tsv"
emit ledger-bad-ids "d.json --ledger ledger.tsv"
mut ledger-changed '.nodes[0].title = "Handles!" | .nodes[1].decided_by = "assistant-best-guess" | .nodes[2].source.extra = "not part of it" | .nodes[3].source.quote = "changed" | .nodes[4].why = "not part of it either"'

# ---------------------------------------------------------------------- credential scans ---
SK="sk-$(rep a1B2 5)"
GH="gh""p_$(rep Ab9 7)"
GHP="github""_pat_$(rep Ab9_ 6)"
AK="AK""IA$(rep ABCD2345 2)"
XOX="xo""xb-$(rep 12ab- 3)"
PK="-----BEGIN RSA PRIV""ATE KEY-----"
PK2="-----BEGIN PRIV""ATE KEY-----"
BEARER="Bea""rer $(rep aB3. 6)"
HEX32="$(rep 0aF9 8)"
OPAQ="$(rep Ab3+ 10)"
OPAQ40="$(rep Ab34 10)"
mut cred-shapes '.nodes[0].why = $t' --arg t "a ${SK} b ${GH} c ${GHP} d ${AK} e ${XOX} f ${PK} g ${PK2} h ${BEARER} i ${HEX32} j ${OPAQ40} k"
mut cred-boundaries '.nodes[0].why = $t' --arg t "x${SK} é${SK} 日${SK} ²${SK} _${SK} -${SK} .${SK} ${AK}X ${AK}é ${AK}- ${AK}5 ${HEX32}g ${HEX32}é ${HEX32}- ${HEX32}${HEX32} $(rep 0aF9 7)0aF ${GH}é"
mut cred-bearer '.nodes[0].why = $t' --arg t "Bea""rer\u00a0$(rep aB3 7) Bea""rer\u3000\u2028$(rep aB3 7) bea""rer $(rep aB3 7) Bea""rer   $(rep aB3 6)a Bea""rer \u0085$(rep aB3 7)"
mut cred-bearer-json '.nodes[0].why = "Bea" + "rer\u00a0" + $a + " | Bea" + "rer\u3000\u2028" + $a + " | Bea" + "rer\u0085" + $a + " | Bea" + "rer\u000b" + $a + " | Bea" + "rer\u001c" + $a + " | Bea" + "rer\u180e" + $a + " | Bea" + "rer\u200b" + $a' --arg a "$(rep aB3 7)"
PW="pass""word"
mut cred-assigned '.nodes[0].why = $t' --arg t "${PW}=abcdef ${PW}: 'abcdefg' PASS""WORD=abcdefgh passwd = \"abcdefgh\" pass=abcdefgh pass:abc sec""ret: [REDACTED-X] tok""en=[redacted-y] tok""en=[ReDaCtEd] api_key=abcdef12 api-key:abcdef12 apikey=abcdef12 api__key=abcdef12 sudo_pw=hunter22 sudopw=hunter22 x${PW}=abcdefgh ${PW}x=abcdefgh é${PW}=abcdefgh ${PW}é=abcdefgh ${PW}=abc;defgh ${PW}=abcd,efgh ${PW}=\"ab\"cdefgh ${PW}='abcdefg' tok""en=''abcdefg sec""ret=:abcdefg"
mut cred-assigned-unicode '.nodes[0].why = "to\u212aen=abcdefgh \u017fecret=abcdefgh ap\u0130_key=abcdefgh ap\u0131key=abcdefgh pa\u017f\u017fword=abcdefgh PA\u017fSWD=abcdefgh token\u3000=\u3000abcdefgh token=\u00a0\u00a0abcdefgh token\u2028:\u2028abcdefgh token=\u200babcdefgh token=abc\u3000defgh token=abcdefg\u00e9\u65e5 token=[\u0280EDACTED] token=[REDACTED token=\"[REDACTED-Q] token=\u00b2token=abcdefgh"'
mut cred-opaque '.nodes[0].why = $t' --arg t "a ${OPAQ40} b .${OPAQ40} c /${OPAQ40} d ${OPAQ40}. e ${OPAQ40}- f ${OPAQ40}== g ${OPAQ40}=== h ${OPAQ40}=x i ${OPAQ}. j +${OPAQ40} k $(rep Ab34 9)Ab3 l ${OPAQ40}+x.y m ${OPAQ40}é n é${OPAQ40} o ${OPAQ40}==. p ${OPAQ40}=. q x+$(rep A 45)+$(rep B 39)+$(rep C 41)-"
mut cred-many '.nodes[0].why = $t | .nodes[1].why = $t | .topics.ops = $t' --arg t "${SK} ${SK} ${SK}"
mut cred-in-keys-and-invalid '.nodes[0][$k] = 1' --arg k "${GH}"
fresh cred-invalid-json
printf '{"why": "%s", "x": ' "${XOX}" >"$W/cred-invalid-json/d.json"
cp "$LEDGER" "$W/cred-invalid-json/ledger.tsv"
emit cred-invalid-json "d.json --ledger ledger.tsv"
mut cred-repr-quote '.nodes[0].why = $t' --arg t "${PW}='ab\"cdefgh ${PW}=ab'cdefg"

# The JSON nesting that json.loads can and cannot recurse into is a CPython stack property,
# not a property of the document: not a parity case (see the main_test.go header).
