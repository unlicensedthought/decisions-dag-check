# decisions-dag-check

A small Go CLI that checks a graph of decisions against an append-only ledger. Standard library only.

It is a port of a Python checker. The Go version reproduces the Python output byte for byte, and the test suite proves it by replaying recorded output files.

## The problem

When a project runs for months, decisions pile up. Some replace earlier ones. Some only partly replace them. Some conflict. Someone later edits an old decision's wording and the record of what was really decided is gone.

This tool keeps that record honest. Each decision is a node. Each node says where it came from and how it relates to earlier nodes. A ledger holds a short fingerprint of every node's provenance. The checker fails when the graph and the ledger disagree.

## Quick start

```sh
go build -o decisions-check ./cmd/decisions-check
./decisions-check --dir examples/decisions
# decisions: ok (5 nodes, 0 open conflicts)
```

Other modes:

```sh
./decisions-check --assemble --dir examples/decisions     # print the one document the directory stands for
./decisions-check --fingerprint --dir examples/decisions  # id<TAB>fingerprint per node
./decisions-check decisions.json --ledger ledger.tsv      # single-file form
./decisions-check --sync-edges decisions.json             # rewrite the edges list (single-file form only)
```

Exit 0 means valid. Exit 1 means problems, one line each on stderr.

## Layout

```
examples/decisions/
  meta.json            generated_at, topics, ingest cursor (everything that is not a node)
  nodes/<id>.json      one node per file; the file name is the id
  ledger/<id>.tsv      that node's ledger lines: id <TAB> fingerprint <TAB> note
```

Edges are not stored. They are derived from the node fields (`depends_on`, `refines`, `implements`, `superseded_by`, `superseded_in_part_by`, `conflicts_with`, `coexists_with`). Two branches that each add a node touch different files, so they merge without a conflict.

## What it checks

- JSON shape, unique ids, every edge target exists, no cycles.
- Every node names its origin. An operator origin cites the transcript line it came from.
- Pasted text is not a decision unless a later turn ratified it.
- Status agrees with edges (superseded needs `superseded_by`, and so on).
- Time runs forward: a node is superseded only by a node at least as new.
- On each topic, a new active directive must be related to the previous one.
- No credential-shaped string anywhere in the document.
- The ledger: a node may not disappear, and its title, decision, decided_by or source may not change, unless a `corrected: <why>` line is appended.

## How the port is tested

`cmd/decisions-check/testdata/scenarios.sh` builds 215 small cases (needs bash, jq and perl). The Python checker ran each case once and its stdout, stderr and exit code were recorded in `testdata/golden/`. `go test` rebuilds the cases, runs the Go code on each, and requires identical bytes.

Three differences are allowed and documented in `main_test.go`: Python tracebacks become `decisions: crash: <ExceptionType>: ...`, the old command name in one hint is mapped to the new one, and a cycle is compared up to rotation (the Python walked a hash-ordered set).

## Limits

- The Python checker is not included, so the goldens cannot be regenerated from this repo.
- Deeply nested JSON that exhausts CPython's recursion limit is not a parity case.
- It validates the structure and provenance of a decisions graph. It does not judge whether a decision is good.

## Tests

```sh
scripts/verify.sh
```

Runs build, vet, gofmt, `go test`, then the built binary on the sample graph (must pass) and on a copy with a node deleted (must fail). The last line is `VERIFY {"verdict":"pass","gates":N}`.

Needs Go 1.24+, bash, jq and perl.

## License

Apache-2.0. See `LICENSE`.
