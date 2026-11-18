package main

// The checks themselves, in check.py's order, so the problems come out in the same order.

import (
	"fmt"
	"math/big"
	"strings"
)

var (
	kinds          = map[string]bool{"directive": true, "feature": true, "architecture": true, "policy": true, "default": true, "deferred": true}
	statuses       = map[string]bool{"active": true, "superseded": true, "partially-superseded": true, "conflict-open": true}
	deciders       = map[string]bool{"operator": true, "assistant-best-guess": true}
	operatorOrigin = map[string]bool{"operator-turn": true, "operator-answer": true, "operator-feedback": true, "relayed": true}
	origins        = map[string]bool{"operator-turn": true, "operator-answer": true, "operator-feedback": true, "relayed": true,
		"pasted-content": true, "global-claude-md": true, "machine-hook": true, "assistant": true}
	// listEdges are the node fields that hold lists of ids, in edge order; depends_on first.
	listEdges = []string{"depends_on", "superseded_in_part_by", "refines", "implements", "conflicts_with", "coexists_with"}
	// directed edges order nodes (checked for cycles with superseded_by).
	directed       = map[string]bool{"depends_on": true, "superseded_in_part_by": true, "refines": true, "implements": true}
	requiredFields = []string{"title", "decision", "why", "kind", "source", "decided_by", "status", "depends_on", "origin", "topics"}
)

// syncEdgesHint names the command that rewrites the edges list. check.py said
// "check.py --sync-edges"; that file is gone (main_test.go maps the old text).
const syncEdgesHint = "decisions-check --sync-edges"

// inStrSet is `v in S` for a set of str: a list or dict raises TypeError (unhashable).
func inStrSet(v any, set map[string]bool) bool {
	h := hkey(v)
	return h[0] == 'S' && set[h[1:]]
}

// --------------------------------------------------------------------- fullmatch helpers ---

// isID is re.fullmatch(r"D-[a-z0-9][a-z0-9-]*", s).
func isID(s string) bool {
	rs := runes(s)
	if len(rs) < 3 || rs[0] != 'D' || rs[1] != '-' {
		return false
	}
	for i, r := range rs[2:] {
		if !(inClass(r, "", rLower, rDigit) || (i > 0 && r == '-')) {
			return false
		}
	}
	return true
}

// isFP is re.fullmatch(r"[0-9a-f]{12}", s).
func isHexN(s string, n int) bool {
	rs := runes(s)
	if len(rs) != n {
		return false
	}
	for _, r := range rs {
		if !inClass(r, "", rDigit, rHexLo) {
			return false
		}
	}
	return true
}

// isTS is TS_RE.fullmatch: \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ with Unicode \d.
func isTS(s string) bool {
	const shape = "dddd-dd-ddTdd:dd:ddZ"
	rs := runes(s)
	if len(rs) != len(shape) {
		return false
	}
	for i, c := range shape {
		if c == 'd' {
			if !isDecimal(rs[i]) {
				return false
			}
		} else if rs[i] != c {
			return false
		}
	}
	return true
}

// refMatch is REF_RE.fullmatch: ([0-9a-f]{8}):L([0-9]+).
func refMatch(s string) (session, line string, ok bool) {
	rs := runes(s)
	if len(rs) < 11 || !isHexN(fromRunes(rs[:8]), 8) || rs[8] != ':' || rs[9] != 'L' {
		return "", "", false
	}
	for _, r := range rs[10:] {
		if !isDigit(r) {
			return "", "", false
		}
	}
	return string(rs[:8]), string(rs[10:]), true
}

// refFull is REF_RE.fullmatch(v) where v must be a str (re raises TypeError otherwise).
func refFull(v any) bool {
	s, ok := v.(string)
	if !ok {
		die("TypeError", "expected string or bytes-like object")
	}
	_, _, m := refMatch(s)
	return m
}

func isDictV(v any) bool { _, ok := v.(*Dict); return ok }
func isListV(v any) bool { _, ok := v.(*List); return ok }

// --------------------------------------------------------------------------- the graph ---

// findCycle returns one cycle in graph, visiting nodes in order. check.py walked a Python set,
// so which rotation of a cycle it printed depended on the per-process string hash seed; the
// port walks node order and is deterministic.
func findCycle(order []string, graph map[string][]string) []string {
	const white, grey, black = 0, 1, 2
	color := map[string]int{}
	for _, k := range order {
		color[k] = white
	}
	var stack []string
	var visit func(u string) []string
	visit = func(u string) []string {
		color[u] = grey
		stack = append(stack, u)
		for _, v := range graph[u] {
			c, ok := color[v]
			if !ok {
				continue
			}
			if c == grey {
				for i, s := range stack {
					if s == v {
						return append(append([]string(nil), stack[i:]...), v)
					}
				}
			}
			if c == white {
				if found := visit(v); found != nil {
					return found
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return nil
	}
	for _, k := range order {
		if color[k] == white {
			if found := visit(k); found != nil {
				return found
			}
		}
	}
	return nil
}

type edge struct {
	from, to any
	typ      string
}

// impliedEdges is implied_edges(nodes, known): every (from, to, type) the node fields imply, in
// discovery order, deduplicated.
func impliedEdges(nodes []any, known hset) ([]edge, hset) {
	var out []edge
	seen := hset{}
	add := func(e edge) {
		k := hkey(e.from) + "\x00" + hkey(e.to) + "\x00" + e.typ
		if !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	for _, n := range nodes {
		node, ok := n.(*Dict)
		if !ok || !known.has(node.get("id")) {
			continue
		}
		nid := sub(node, "id")
		for _, f := range listEdges {
			for _, t := range iterOr(node.get(f)) {
				if known.has(t) {
					add(edge{nid, t, f})
				}
			}
		}
		if sup := node.get("superseded_by"); known.has(sup) {
			add(edge{nid, sup, "superseded_by"})
		}
	}
	return out, seen
}

func edgeKey(from, to any, typ any) string {
	return hkey(from) + "\x00" + hkey(to) + "\x00" + hkeyTyp(typ)
}

// hkeyTyp keys an edge type the way impliedEdges does (a plain str), and anything else apart.
func hkeyTyp(v any) string {
	h := hkey(v)
	if h[0] == 'S' {
		return h[1:]
	}
	return "\x01" + h
}

// ---------------------------------------------------------------------------- the ledger ---

// splitlines is str.splitlines().
func splitlines(s string) []string {
	var out []string
	rs := runes(s)
	start := 0
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\r', 0x0b, 0x0c, 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, fromRunes(rs[start:i]))
			if rs[i] == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, fromRunes(rs[start:]))
	}
	return out
}

func blank(s string) bool {
	for _, r := range runes(s) {
		if !isSpace(r) {
			return false
		}
	}
	return true
}

func checkLedger(nodes []any, text, where string) []string {
	var problems []string
	var latestOrder []string
	latest := map[string]string{}
	seen := map[string]bool{}
	for ln, line := range splitlines(text) {
		ln++
		if blank(line) || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || !isID(parts[0]) || !isHexN(parts[1], 12) {
			problems = append(problems, fmt.Sprintf("%s line %d: want id<TAB>fingerprint<TAB>note", where, ln))
			continue
		}
		nid, fp, note := parts[0], parts[1], parts[2]
		if seen[nid] && !strings.HasPrefix(note, "corrected:") {
			problems = append(problems, fmt.Sprintf("%s line %d: %s changed without a 'corrected: <why>' note", where, ln, nid))
		}
		seen[nid] = true
		if _, ok := latest[nid]; !ok {
			latestOrder = append(latestOrder, nid)
		}
		latest[nid] = fp
	}
	byID := newHdict()
	for _, n := range nodes {
		if d, ok := n.(*Dict); ok {
			byID.set(d.get("id"), d)
		}
	}
	for _, nid := range latestOrder {
		fp := latest[nid]
		v, ok := byID.val["S"+nid]
		if !ok {
			problems = append(problems, fmt.Sprintf("ledger: %s was removed from the DAG (history is append-only: supersede it instead)", nid))
		} else if got := fingerprint(v.(*Dict)); got != fp {
			problems = append(problems, fmt.Sprintf("ledger: %s changed its title/decision/decided_by/source (fingerprint %s, ledger %s): append '%s<TAB>%s<TAB>corrected: <why>' or revert", nid, got, fp, nid, fingerprint(v.(*Dict))))
		}
	}
	for _, h := range byID.order {
		k := byID.key[h]
		if !truthy(k) {
			continue
		}
		if s, ok := k.(string); ok {
			if _, in := latest[s]; in {
				continue
			}
		}
		problems = append(problems, fmt.Sprintf("ledger: %s is not in the ledger (append '%s<TAB>%s<TAB>added')", str(k), str(k), fingerprint(byID.val[h].(*Dict))))
	}
	return problems
}

// ----------------------------------------------------------------------------- check() ---

// check is check(raw, ledger_text, ledger_where); ledger nil means the ledger file is missing.
func check(raw string, ledger *string, ledgerWhere string) []string {
	problems := checkText(raw)
	p := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	v, err := loads(raw)
	if err != nil {
		return append(problems, "invalid JSON: "+err.Error())
	}
	doc, ok := v.(*Dict)
	if !ok || !isListV(doc.get("nodes")) {
		return append(problems, "top level must be an object with a 'nodes' list")
	}
	nodes := doc.get("nodes").(*List).items
	if !truthy(doc.get("generated_at")) {
		p("missing generated_at")
	}
	topics, ok := doc.get("topics").(*Dict)
	if !ok || len(topics.keys) == 0 {
		p("missing topics (an object of topic slug -> what it covers)")
		topics = newDict()
	}

	var ids []string
	for i, n := range nodes {
		where := fmt.Sprintf("node #%d", i)
		node, ok := n.(*Dict)
		if !ok {
			p("%s is not an object", where)
			continue
		}
		nidV := node.get("id")
		nid, isStr := nidV.(string)
		if !isStr || !isID(nid) {
			p("%s has a bad id %s (want D-<slug>)", where, repr(nidV))
			continue
		}
		where = nid
		ids = append(ids, nid)
		for _, f := range requiredFields {
			if !node.has(f) {
				p("%s: missing %s", where, f)
			}
		}
		if !inStrSet(node.get("kind"), kinds) {
			p("%s: bad kind %s", where, repr(node.get("kind")))
		}
		status := node.get("status")
		if !inStrSet(status, statuses) {
			p("%s: bad status %s", where, repr(status))
		}
		if !inStrSet(node.get("decided_by"), deciders) {
			p("%s: bad decided_by %s", where, repr(node.get("decided_by")))
		}
		var src *Dict
		if s, ok := node.get("source").(*Dict); ok {
			src = s
			for _, f := range []string{"session", "timestamp", "quote"} {
				if !truthy(src.get(f)) {
					p("%s: source.%s missing", where, f)
				}
			}
			if pyLen(src.getOr("quote", "")) > 200 {
				p("%s: quote longer than 200 chars", where)
			}
			if ts := src.get("timestamp"); truthy(ts) {
				s, ok := ts.(string)
				if !ok {
					die("TypeError", "expected string or bytes-like object")
				}
				if !isTS(s) {
					p("%s: source.timestamp %s is not ISO-8601 UTC (YYYY-MM-DDTHH:MM:SSZ)", where, repr(s))
				}
			}
			refs := append([]any{src.get("ref")}, iterOr(src.get("refs"))...)
			for _, r := range refs {
				if r != nil {
					if _, _, m := refMatch(str(r)); !m {
						p("%s: transcript ref %s is not <session8>:L<line>", where, repr(r))
					}
				}
			}
			if ref := src.get("ref"); truthy(ref) {
				head := runes(str(ref))
				if len(head) > 8 {
					head = head[:8]
				}
				if !strings.HasPrefix(str(src.getOr("session", "")), fromRunes(head)) {
					p("%s: source.ref %s is not in source.session %s", where, str(ref), str(src.get("session")))
				}
			}
		} else if node.has("source") {
			p("%s: source must be an object", where)
			src = newDict()
		} else {
			src = newDict()
		}
		if !isListV(node.get("depends_on")) {
			p("%s: depends_on must be a list", where)
		}
		for _, f := range listEdges[1:] {
			if node.has(f) && (!isListV(node.get(f)) || !truthy(node.get(f))) {
				p("%s: %s must be a non-empty list when present", where, f)
			}
		}
		if truthy(node.get("unknown")) && !truthy(node.get("best_guess_reason")) {
			p("%s: unknown without best_guess_reason", where)
		}
		if truthy(node.get("unknown")) && !eq(node.get("decided_by"), "assistant-best-guess") {
			p("%s: unknown must be decided_by assistant-best-guess", where)
		}
		if eq(node.get("decided_by"), "assistant-best-guess") && !truthy(node.get("best_guess_reason")) {
			p("%s: assistant-best-guess without best_guess_reason", where)
		}

		// Where it came from, and who may be said to have decided it.
		origin := node.get("origin")
		if !inStrSet(origin, origins) {
			var names []string
			for o := range origins {
				names = append(names, o)
			}
			p("%s: bad origin %s (want one of %s)", where, repr(origin), strings.Join(sortedStrings(names), ", "))
		}
		if inStrSet(origin, operatorOrigin) {
			if !truthy(src.get("ref")) {
				p("%s: origin %s needs source.ref, the transcript turn it came from", where, str(origin))
			}
			if !eq(node.get("decided_by"), "operator") {
				p("%s: origin %s must be decided_by operator", where, str(origin))
			}
		}
		if eq(origin, "assistant") && !eq(node.get("decided_by"), "assistant-best-guess") {
			p("%s: origin assistant must be decided_by assistant-best-guess", where)
		}
		if eq(origin, "pasted-content") && eq(node.get("decided_by"), "operator") && !truthy(node.get("ratified_by")) {
			p("%s: pasted text is not the operator's decision; decided_by operator needs ratified_by (the operator turn that adopted it)", where)
		}
		if rb := node.get("ratified_by"); truthy(rb) {
			if _, _, m := refMatch(str(rb)); !m {
				p("%s: ratified_by %s is not <session8>:L<line>", where, repr(rb))
			} else {
				refs := src.get("refs")
				if !truthy(refs) {
					refs = &List{}
				}
				if !in(rb, refs) {
					p("%s: ratified_by %s must also be listed in source.refs", where, str(rb))
				}
			}
		}

		tl := node.get("topics")
		if !isListV(tl) || !truthy(tl) {
			p("%s: topics must be a non-empty list", where)
		} else {
			for _, t := range tl.(*List).items {
				if !in(t, topics) {
					p("%s: topic %s is not declared in topics", where, repr(t))
				}
			}
		}

		// Status must agree with the edges.
		if eq(status, "superseded") && !truthy(node.get("superseded_by")) {
			p("%s: superseded without superseded_by", where)
		}
		if truthy(node.get("superseded_by")) && !eq(status, "superseded") {
			p("%s: has superseded_by but status is not superseded", where)
		}
		if eq(status, "partially-superseded") {
			if !truthy(node.get("superseded_in_part_by")) {
				p("%s: partially-superseded without superseded_in_part_by", where)
			}
			if !truthy(node.get("remaining")) {
				p("%s: partially-superseded must say what still holds (remaining)", where)
			}
		} else if truthy(node.get("superseded_in_part_by")) && !eq(status, "superseded") {
			p("%s: has superseded_in_part_by but status is %s, not partially-superseded", where, str(status))
		}
		c := node.get("conflict")
		if eq(status, "conflict-open") {
			if !truthy(node.get("conflicts_with")) {
				p("%s: conflict-open without conflicts_with", where)
			}
			cd, ok := c.(*Dict)
			if !ok || !truthy(cd.get("question")) || !truthy(cd.get("best_guess")) {
				p("%s: conflict-open needs conflict.question (for the operator) and conflict.best_guess (what is built meanwhile)", where)
			}
		} else if truthy(node.get("conflicts_with")) || truthy(c) {
			p("%s: conflicts_with/conflict present but status is %s, not conflict-open", where, str(status))
		}
	}

	seen := map[string]bool{}
	for _, nid := range ids {
		if seen[nid] {
			p("duplicate id %s", nid)
		}
		seen[nid] = true
	}

	known := hset{}
	for _, id := range ids {
		known.add(id)
	}
	var order []string
	byID := map[string]*Dict{}
	for _, n := range nodes {
		node, ok := n.(*Dict)
		if !ok || !known.has(node.get("id")) {
			continue
		}
		id := node.get("id").(string)
		if _, dup := byID[id]; !dup {
			order = append(order, id)
		}
		byID[id] = node
	}
	ts := func(id string) any {
		s, ok := byID[id].get("source").(*Dict)
		if !ok {
			return ""
		}
		return s.getOr("timestamp", "")
	}
	graph := map[string][]string{}
	for _, nid := range order {
		node := byID[nid]
		for _, f := range listEdges {
			for _, t := range iterOr(node.get(f)) {
				switch {
				case eq(t, nid):
					p("%s: %s itself", nid, f)
				case !known.has(t):
					p("%s: %s references missing id %s", nid, f, str(t))
				case directed[f]:
					graph[nid] = append(graph[nid], t.(string))
				}
			}
		}
		sup := node.get("superseded_by")
		if truthy(sup) {
			if !known.has(sup) {
				p("%s: superseded_by references missing id %s", nid, str(sup))
			} else {
				graph[nid] = append(graph[nid], sup.(string))
			}
		}
		// Time runs forward.
		var later []string
		if known.has(sup) {
			later = append(later, sup.(string))
		}
		for _, x := range iterOr(node.get("superseded_in_part_by")) {
			if known.has(x) {
				later = append(later, x.(string))
			}
		}
		for _, t := range later {
			if lt(ts(t), ts(nid)) {
				p("%s (%s) is superseded by the OLDER %s (%s): a newer directive cannot be replaced by an older one", nid, str(ts(nid)), t, str(ts(t)))
			}
		}
		for _, t := range iterOr(node.get("refines")) {
			if known.has(t) && gt(ts(t.(string)), ts(nid)) {
				p("%s (%s) refines the NEWER %s (%s): refine only what came before", nid, str(ts(nid)), t, str(ts(t.(string))))
			}
		}
		for _, t := range iterOr(node.get("conflicts_with")) {
			if known.has(t) && eq(byID[t.(string)].get("status"), "superseded") {
				p("%s: conflicts_with %s, which is superseded: the conflict is resolved, close it", nid, t)
			}
		}
	}

	if cyc := findCycle(order, graph); cyc != nil {
		p("cycle: %s", strings.Join(cyc, " -> "))
	}

	// On every topic, each active directive/policy is related to the one before it.
	implied, impliedSet := impliedEdges(nodes, known)
	related := map[[2]string]bool{}
	for _, e := range implied {
		a, b := e.from.(string), e.to.(string)
		related[[2]string{a, b}] = true
		related[[2]string{b, a}] = true
	}
	for _, topic := range sortedStrings(topics.keys) {
		var live []string
		for _, id := range order {
			n := byID[id]
			tv := n.get("topics")
			if !truthy(tv) {
				tv = &List{}
			}
			if in(topic, tv) && (eq(n.get("kind"), "directive") || eq(n.get("kind"), "policy")) && !eq(n.get("status"), "superseded") {
				live = append(live, id)
			}
		}
		sortLive(live, ts)
		for i := 1; i < len(live); i++ {
			prev, cur := live[i-1], live[i]
			if !related[[2]string{prev, cur}] {
				p("topic %s: %s (%s) and the earlier %s (%s) are both active and unrelated: say how the newer one relates (supersede, refines, superseded_in_part_by, conflicts_with or coexists_with)",
					topic, cur, str(ts(cur)), prev, str(ts(prev)))
			}
		}
	}
	used := hset{}
	for _, id := range order {
		for _, t := range iterOr(byID[id].get("topics")) {
			used.add(t)
		}
	}
	var unused []string
	for _, t := range topics.keys {
		if !used.has(t) {
			unused = append(unused, t)
		}
	}
	for _, t := range sortedStrings(unused) {
		p("topic %s is declared but no node uses it", reprStr(t))
	}

	ing, ok := doc.get("ingest").(*Dict)
	if !ok || !isDictV(ing.get("cursor")) || !isListV(ing.get("triaged")) {
		p("missing ingest {cursor: {session8: line}, triaged: [...]} (see web/cmd/directive-turns)")
	} else {
		cursor := ing.get("cursor").(*Dict)
		for _, s := range cursor.keys {
			line := cursor.m[s]
			_, isBool := line.(bool)
			_, isInt := line.(*big.Int)
			if !isHexN(s, 8) || !(isBool || isInt) || lt(line, big.NewInt(1)) {
				p("ingest.cursor: %s -> %s is not session8 -> line", reprStr(s), repr(line))
			}
		}
		cited := hset{}
		for _, id := range order {
			src := byID[id].get("source")
			if !truthy(src) {
				src = newDict()
			}
			first := dictGet(src, "ref")
			rest := iterOr(dictGet(src, "refs"))
			for _, r := range append([]any{first}, rest...) {
				if truthy(r) {
					cited.add(r)
				}
			}
		}
		seenT := map[string]bool{}
		for _, t := range ing.get("triaged").(*List).items {
			var r any
			td, isD := t.(*Dict)
			if isD {
				r = td.get("ref")
			}
			if !truthy(r) || !refFull(r) || !truthy(td.get("reason")) {
				p("ingest.triaged: %s needs ref <session8>:L<line> and a reason", repr(t))
				continue
			}
			rs := r.(string)
			if seenT[rs] {
				p("ingest.triaged: %s listed twice", rs)
			}
			if cited.has(rs) {
				p("ingest.triaged: %s is already cited by a node; triage is for turns that hold no directive", rs)
			}
			seenT[rs] = true
			if cb := td.get("covered_by"); truthy(cb) && !known.has(cb) {
				p("ingest.triaged: %s covered_by missing id %s", rs, str(cb))
			}
			session, line, _ := refMatch(rs)
			ln, _ := new(big.Int).SetString(line, 10)
			if lt(cursor.getOr(session, big.NewInt(0)), ln) {
				p("ingest.triaged: %s is past the cursor for %s", rs, session)
			}
		}
	}

	if doc.has("edges") {
		listed := hset{}
		for _, e := range iter(doc.get("edges")) {
			if ed, ok := e.(*Dict); ok {
				listed[edgeKey(ed.get("from"), ed.get("to"), ed.get("type"))] = true
			}
		}
		if !listed.equal(impliedSet) {
			p("edges do not match the node fields (run %s)", syncEdgesHint)
		}
	} else {
		p("missing edges (run %s)", syncEdgesHint)
	}

	if ledger == nil {
		p("ledger missing: %s", ledgerWhere)
	} else {
		problems = append(problems, checkLedger(nodes, *ledger, ledgerWhere)...)
	}
	return problems
}

// sortLive is sorted(live, key=lambda n: (ts(n["id"]), n["id"])). The timestamps that reach here
// are strs or falsy non-strs (a truthy non-str crashed in the node loop). Inside one kind they
// compare (or are equal, so the id decides); two kinds in one list cannot be ordered, and any
// comparison sort must compare across them, so CPython raises TypeError.
func sortLive(live []string, ts func(string) any) {
	if len(live) < 2 {
		return
	}
	class := func(v any) string {
		if isNum(v) {
			return "number"
		}
		return typeName(v)
	}
	first := ts(live[0])
	for _, id := range live[1:] {
		if v := ts(id); class(v) != class(first) {
			die("TypeError", "'<' not supported between instances of '%s' and '%s'", typeName(v), typeName(first))
		}
	}
	_, strs := first.(string)
	sortStable(live, func(a, b string) bool {
		if strs {
			ta, tb := ts(a).(string), ts(b).(string)
			if ta != tb {
				return ta < tb
			}
		}
		return a < b
	})
}
