// Command decisions-check validates a decisions graph (a DAG of decisions, each with its origin
// and its relations to earlier ones) against an append-only provenance ledger. It is the Go port
// of a retired Python checker and reproduces its output byte for byte (see main_test.go and
// testdata/golden/). Stdlib only.
//
//	decisions-check [decisions.json] [--ledger ledger.tsv]
//	decisions-check --sync-edges [decisions.json]     rewrite the edges list from the nodes
//	decisions-check --fingerprint [decisions.json]    print "id<TAB>fingerprint" per node
//	decisions-check [--fingerprint|--assemble] --dir <dir>   the per-node layout (the default when <dir>/meta.json exists)
//
// Layout (dir.go): meta.json holds everything but the nodes, nodes/<id>.json one node each,
// ledger/<id>.tsv that node's ledger lines; edges are derived. Two branches that each add a node
// touch different files, so they merge without a conflict. The single-file form above stays for
// fixtures; --assemble prints the one document the directory stands for.
//
// The default paths point at $DECISIONS_DIR (without it, ./docs/decisions).
//
// Exit 0 when valid. Otherwise one line per problem on stderr and exit 1. A document the Python checker
// could not survive (a value of a type it did not expect, an unreadable file) still exits 1,
// with "decisions: crash: <PythonExceptionType>: <detail>" on stderr.
//
// Checks: valid JSON and shape, unique ids, every edge target exists, no cycles, unknowns carry
// a best-guess reason, edges match the nodes, no credential-like string anywhere, and the
// directive rules that keep a newer instruction from being conflated with an older one:
//
//   - every node says where it came from (origin) and an operator origin cites the transcript
//     turn it came from (source.ref, "<session8>:L<line>");
//   - pasted text (a guide the operator pasted) is not the operator's decision unless a later
//     operator turn ratified it;
//   - status agrees with the edges: superseded <-> superseded_by, partially-superseded <->
//     superseded_in_part_by plus what still holds (remaining), conflict-open <-> conflicts_with
//     plus the question to put to the operator;
//   - time runs forward: a node is superseded only by a node at least as new, and refines only
//     nodes at least as old;
//   - on every topic, each active directive or policy is explicitly related to the active one
//     before it (depends_on, refines, implements, superseded_in_part_by, coexists_with or
//     conflicts_with): adding a directive forces a decision about how it relates to the last one
//     on that topic;
//   - the ledger is the append-only record of every node's provenance: a node may not disappear,
//     and its title, decision, decided_by or source may not change, unless a "corrected:" line
//     says why.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// output buffers stdout the way a Python process does: what was printed before a crash still
// comes out. Stdout is strict UTF-8 (a lone surrogate raises); stderr backslash-escapes it.
type output struct{ out, err bytes.Buffer }

func (o *output) println(s string) { o.out.Write(utf8Strict(s)); o.out.WriteByte('\n') }

func (o *output) eprintln(s string) {
	for _, r := range runes(s) {
		if isSurrogate(r) {
			fmt.Fprintf(&o.err, "\\u%04x", r)
		} else {
			o.err.Write(utf8.AppendRune(nil, r))
		}
	}
	o.err.WriteByte('\n')
}

func run(args []string, stdout, stderr io.Writer) (code int) {
	o := &output{}
	defer func() {
		if r := recover(); r != nil {
			c, ok := r.(crash)
			if !ok {
				panic(r)
			}
			o.eprintln("decisions: crash: " + c.typ + ": " + c.msg)
			code = 1
		}
		_, _ = stdout.Write(o.out.Bytes())
		_, _ = stderr.Write(o.err.Bytes())
	}()
	return mainArgs(append([]string(nil), args...), o)
}

func mainArgs(args []string, o *output) int {
	here := os.Getenv("DECISIONS_DIR")
	if here == "" {
		here = filepath.Join("docs", "decisions")
	}
	mode := ""
	if len(args) > 0 && (args[0] == "--sync-edges" || args[0] == "--fingerprint") {
		mode, args = args[0], args[1:]
	}
	dirArg := ""
	for k, a := range args {
		if a == "--dir" {
			if k+1 >= len(args) {
				die("IndexError", "list index out of range")
			}
			dirArg = args[k+1]
			args = append(args[:k:k], args[k+2:]...)
			break
		}
	}
	if mode == "" && len(args) > 0 && args[0] == "--assemble" {
		mode, args = args[0], args[1:]
	}
	ledger := filepath.Join(here, "ledger.tsv")
	for k, a := range args {
		if a == "--ledger" {
			if k+1 >= len(args) {
				die("IndexError", "list index out of range")
			}
			ledger = args[k+1]
			args = append(args[:k:k], args[k+2:]...)
			break
		}
	}
	path := filepath.Join(here, "decisions.json")
	if len(args) > 0 {
		path = args[0]
	}
	if dirArg == "" && len(args) == 0 {
		if _, err := os.Stat(filepath.Join(here, "meta.json")); err == nil {
			dirArg = here
		}
	}
	if dirArg != "" {
		return dirMode(dirArg, mode, args, ledger != filepath.Join(here, "ledger.tsv"), o)
	}
	if mode == "--assemble" {
		o.eprintln("decisions: --assemble needs a directory layout (meta.json, nodes/, ledger/)")
		return 1
	}
	if mode == "--sync-edges" {
		return syncEdges(path, o)
	}
	raw := mustRead(path)
	if mode == "--fingerprint" {
		for _, n := range iter(sub(mustLoads(raw), "nodes")) {
			id := sub(n, "id")
			o.println(str(id) + "\t" + fingerprint(n.(*Dict)))
		}
		return 0
	}
	var ledgerText *string
	if t, err := readText(ledger); err == nil {
		ledgerText = &t
	} else if !errors.As(err, new(*fs.PathError)) {
		panic(err)
	}
	problems := check(raw, ledgerText, ledger)
	for _, p := range problems {
		o.eprintln("decisions: " + p)
	}
	if len(problems) > 0 {
		return 1
	}
	nodes := sub(mustLoads(raw), "nodes").(*List).items
	open := 0
	for _, x := range nodes {
		if eq(x.(*Dict).get("status"), "conflict-open") {
			open++
		}
	}
	o.println(fmt.Sprintf("decisions: ok (%d nodes, %d open conflicts)", len(nodes), open))
	return 0
}

// readText is open(path, encoding="utf-8").read(): strict UTF-8, universal newlines. An OS error
// comes back as a *fs.PathError (Python's OSError); undecodable bytes crash (UnicodeDecodeError
// is not an OSError, so check.py did not catch it for the ledger either).
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			err = &fs.PathError{Op: "open", Path: path, Err: err}
		}
		return "", err
	}
	if !utf8.Valid(b) {
		pos := 0
		for pos < len(b) {
			r, n := utf8.DecodeRune(b[pos:])
			if r == utf8.RuneError && n <= 1 {
				break
			}
			pos += n
		}
		die("UnicodeDecodeError", "'utf-8' codec can't decode byte 0x%02x in position %d", b[pos], pos)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n"), nil
}

// mustRead is readText where check.py let the OSError escape.
func mustRead(path string) string {
	s, err := readText(path)
	if err != nil {
		var pe *fs.PathError
		errors.As(err, &pe)
		typ := "OSError"
		switch {
		case errors.Is(pe.Err, fs.ErrNotExist):
			typ = "FileNotFoundError"
		case errors.Is(pe.Err, syscall.EISDIR):
			typ = "IsADirectoryError"
		case errors.Is(pe.Err, fs.ErrPermission):
			typ = "PermissionError"
		}
		die(typ, "%s: %s", pe.Err, reprStr(path))
	}
	return s
}

// syncEdges rewrites the edges list from the node fields, ordered by the from node's position,
// then the edge type, then the to node's position.
func syncEdges(path string, o *output) int {
	doc := mustLoads(mustRead(path))
	nodes := iter(sub(doc, "nodes"))
	known := hset{}
	for _, n := range nodes {
		known.add(sub(n, "id"))
	}
	edges, _ := impliedEdges(nodes, known)
	list := edgeList(nodes, edges)
	d, ok := doc.(*Dict)
	if !ok {
		die("TypeError", "'%s' object does not support item assignment", typeName(doc))
	}
	d.set("edges", list)
	fh, err := os.Create(path)
	if err != nil {
		mustRead(path) // the same OSError check.py raised on open(path, "w")
		panic(err)
	}
	defer fh.Close()
	text := string(dumpsIndent2(nil, d, 0)) + "\n"
	if _, err := fh.Write(utf8Strict(text)); err != nil {
		panic(err)
	}
	o.println(fmt.Sprintf("decisions: wrote %d edges to %s", len(edges), path))
	return 0
}

// edgeList is the edges list for nodes: ordered by the from node's position, then the edge type,
// then the to node's position.
func edgeList(nodes []any, edges []edge) *List {
	order := map[string]int{}
	for i, t := range append([]string{"depends_on", "superseded_by"}, listEdges[1:]...) {
		order[t] = i
	}
	pos := map[string]int{}
	for k, n := range nodes {
		pos[hkey(sub(n, "id"))] = k
	}
	sortStable(edges, func(a, b edge) bool {
		ka := [3]int{pos[hkey(a.from)], order[a.typ], pos[hkey(a.to)]}
		kb := [3]int{pos[hkey(b.from)], order[b.typ], pos[hkey(b.to)]}
		for i := range ka {
			if ka[i] != kb[i] {
				return ka[i] < kb[i]
			}
		}
		return false
	})
	list := &List{}
	for _, e := range edges {
		d := newDict()
		d.set("from", e.from)
		d.set("to", e.to)
		d.set("type", e.typ)
		list.items = append(list.items, d)
	}
	return list
}
