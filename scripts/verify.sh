#!/usr/bin/env bash
# Build, vet, format-check and test decisions-check, then run the built binary against the
# sample graph (must pass) and against a broken copy of it (must fail). Last line is the verdict.
set -u
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)" || exit 2
cd "$ROOT" || exit 2
gates=0
fail() { echo "VERIFY {\"verdict\":\"fail\",\"failed\":\"$1\"}"; exit 1; }
step() { # step <name> <cmd...>
	local name="$1"; shift
	echo "== $name"
	"$@" || fail "$name"
	gates=$((gates + 1))
}

for tool in go bash jq perl; do
	command -v "$tool" >/dev/null 2>&1 || fail "need-$tool"
done

bin="$(mktemp -d)/decisions-check"
trap 'rm -rf "$(dirname "$bin")"' EXIT

fmt_clean() { [ -z "$(gofmt -l .)" ]; }
sample_ok() { out="$("$bin" --dir examples/decisions 2>&1)" && echo "$out" && [ "${out#decisions: ok (5 nodes, 0 open conflicts)}" != "$out" ]; }
broken_fails() {
	local tmp rc
	tmp="$(mktemp -d)"
	cp -R examples/decisions/. "$tmp/"
	rm "$tmp/nodes/D-kick.json"
	"$bin" --dir "$tmp" 2>"$tmp/err"
	rc=$?
	cat "$tmp/err"
	grep -q 'D-kick was removed from the DAG' "$tmp/err" && [ "$rc" -eq 1 ]
}

step build go build -o "$bin" ./cmd/decisions-check
step vet go vet ./...
step gofmt fmt_clean
step test go test -count=1 ./...
step sample-passes sample_ok
step broken-sample-fails broken_fails

echo "VERIFY {\"verdict\":\"pass\",\"gates\":$gates}"
