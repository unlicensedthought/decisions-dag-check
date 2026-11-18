package main

// Directory layout: the DAG as one file per node, so two branches that each add a node touch
// different files and merge without a conflict.
//
//	<dir>/meta.json            generated_at, sources, topics, ingest (everything that is not a node)
//	<dir>/nodes/<id>.json      one node, the file name is the id
//	<dir>/ledger/<id>.tsv      that node's lines of the append-only ledger (id<TAB>fingerprint<TAB>note)
//
// edges are not stored: they follow from the node fields and are derived on load. Nodes are
// assembled in id order (bytewise), so the result does not depend on the directory listing.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tryLoads is mustLoads that returns the parser's message instead of panicking.
func tryLoads(text string) (v any, msg string) {
	defer func() {
		if r := recover(); r != nil {
			c, ok := r.(crash)
			if !ok {
				panic(r)
			}
			v, msg = nil, c.typ+": "+c.msg
		}
	}()
	return mustLoads(text), ""
}

func readFileText(path string) (text, msg string) {
	defer func() {
		if r := recover(); r != nil {
			c, ok := r.(crash)
			if !ok {
				panic(r)
			}
			text, msg = "", c.typ+": "+c.msg
		}
	}()
	return mustRead(path), ""
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// loadDir assembles dir into the single-document text check() takes, and the concatenated
// ledger. Problems that stop assembly (a node file that is not JSON, not an object, or named
// for another id) come back as lines for stderr; nothing else is checked here.
func loadDir(dir string) (doc, ledger string, problems []string) {
	metaPath := filepath.Join(dir, "meta.json")
	metaText, msg := readFileText(metaPath)
	if msg != "" {
		return "", "", []string{"meta.json: " + msg}
	}
	meta, msg := tryLoads(metaText)
	if msg != "" {
		return "", "", []string{"meta.json: " + msg}
	}
	md, ok := meta.(*Dict)
	if !ok {
		return "", "", []string{"meta.json: want a JSON object"}
	}
	if md.has("nodes") || md.has("edges") {
		problems = append(problems, "meta.json: nodes and edges live in nodes/<id>.json and are derived; remove them from meta.json")
	}

	files, _ := filepath.Glob(filepath.Join(dir, "nodes", "*"))
	sortByStem(files)
	list := &List{}
	seen := map[string]bool{}
	for _, f := range files {
		base := filepath.Base(f)
		stem, isJSON := strings.CutSuffix(base, ".json")
		if !isJSON {
			problems = append(problems, "nodes/"+base+": not a <id>.json file")
			continue
		}
		text, msg := readFileText(f)
		var v any
		if msg == "" {
			v, msg = tryLoads(text)
		}
		if msg != "" {
			problems = append(problems, "nodes/"+base+": "+msg)
			continue
		}
		d, ok := v.(*Dict)
		if !ok {
			problems = append(problems, "nodes/"+base+": want a JSON object (one node)")
			continue
		}
		if id, _ := d.get("id").(string); id != stem {
			problems = append(problems, fmt.Sprintf("nodes/%s: id is %s, the file name says %q", base, repr(d.get("id")), stem))
			continue
		}
		seen[stem] = true
		list.items = append(list.items, d)
	}

	lfiles, _ := filepath.Glob(filepath.Join(dir, "ledger", "*"))
	sortByStem(lfiles)
	var lb strings.Builder
	for _, f := range lfiles {
		base := filepath.Base(f)
		stem, isTSV := strings.CutSuffix(base, ".tsv")
		if !isTSV {
			problems = append(problems, "ledger/"+base+": not a <id>.tsv file")
			continue
		}
		text, msg := readFileText(f)
		if msg != "" {
			problems = append(problems, "ledger/"+base+": "+msg)
			continue
		}
		for ln, line := range splitlines(text) {
			if blank(line) || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, "\t")
			switch {
			case len(parts) != 3 || !isID(parts[0]) || !isHexN(parts[1], 12):
				problems = append(problems, fmt.Sprintf("ledger/%s line %d: want id<TAB>fingerprint<TAB>note", base, ln+1))
			case parts[0] != stem:
				problems = append(problems, fmt.Sprintf("ledger/%s line %d: id %q belongs in ledger/%s.tsv", base, ln+1, parts[0], parts[0]))
			}
		}
		lb.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			lb.WriteByte('\n')
		}
	}
	if len(problems) > 0 {
		return "", "", problems
	}

	md.set("nodes", list)
	known := hset{}
	for _, n := range list.items {
		known.add(sub(n, "id"))
	}
	edges, _ := impliedEdges(list.items, known)
	md.set("edges", edgeList(list.items, edges))
	return string(dumpsIndent2(nil, md, 0)) + "\n", lb.String(), nil
}

// dirMode runs a mode against the directory layout. A flag or argument the layout has no use
// for is refused rather than ignored.
func dirMode(dir, mode string, args []string, ledgerFlag bool, o *output) int {
	fail := func(ps ...string) int {
		for _, p := range ps {
			o.eprintln("decisions: " + p)
		}
		return 1
	}
	switch {
	case mode == "--sync-edges":
		return fail("--sync-edges: edges are derived from nodes/ in directory mode; there is nothing to write")
	case ledgerFlag:
		return fail("--ledger: the ledger is ledger/<id>.tsv in directory mode")
	case len(args) > 0:
		return fail("unexpected argument " + reprStr(args[0]) + " with --dir")
	}
	doc, ledger, problems := loadDir(dir)
	if len(problems) > 0 {
		return fail(problems...)
	}
	switch mode {
	case "--assemble":
		o.out.WriteString(doc)
		return 0
	case "--fingerprint":
		for _, n := range iter(sub(mustLoads(doc), "nodes")) {
			o.println(str(sub(n, "id")) + "\t" + fingerprint(n.(*Dict)))
		}
		return 0
	}
	problems = check(doc, &ledger, filepath.Join(dir, "ledger"))
	for _, p := range problems {
		o.eprintln("decisions: " + p)
	}
	if len(problems) > 0 {
		return 1
	}
	nodes := sub(mustLoads(doc), "nodes").(*List).items
	open := 0
	for _, x := range nodes {
		if eq(x.(*Dict).get("status"), "conflict-open") {
			open++
		}
	}
	o.println(fmt.Sprintf("decisions: ok (%d nodes, %d open conflicts)", len(nodes), open))
	return 0
}

// sortByStem orders files by name without the extension, i.e. by id: "D-a" before "D-a-b",
// where sorting "D-a.json" against "D-a-b.json" would put them the other way round.
func sortByStem(files []string) {
	stem := func(f string) string { return strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) }
	sortStable(files, func(a, b string) bool { return stem(a) < stem(b) })
}
