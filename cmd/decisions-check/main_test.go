package main

// Parity with the retired Python checker (check.py).
//
// TestGoldenParity runs testdata/scenarios.sh (the same builder that fed check.py when the
// goldens were recorded) and replays every case through run(), requiring byte-identical stdout,
// exit code and stderr, and an identical sync.json for --sync-edges cases. Three documented
// exceptions, applied to the golden before comparing:
//
//  1. A case where check.py died with a traceback keeps only the exception line in its golden
//     ("TypeError: unhashable type: 'list'"); the port must exit 1 with
//     "decisions: crash: <same exception type>: ..." on stderr and the same stdout.
//  2. The edges hint named "check.py --sync-edges"; it now names decisions-check --sync-edges.
//  3. check.py's cycle search walked a Python set, so the rotation of a cycle it printed
//     depended on the per-process string hash seed (python3 -I randomizes it). Cycle lines are
//     compared as cycles: rotated to start at their smallest id.
//
// Not a parity case: JSON nested deep enough to exhaust CPython's recursion limit (a stack
// property of the interpreter, not of the document; Go recurses without that limit).
//
// TestUnicodeTables checks the character classes the port uses against CPython 3.9's, recorded
// in testdata/py39-unicode.txt, on every code point that Unicode 13.0 (CPython 3.9's database)
// assigns. Go's tables are newer; the two differ only on code points 13.0 left unassigned.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func canonCycle(line string) string {
	const pre = "decisions: cycle: "
	if !strings.HasPrefix(line, pre) {
		return line
	}
	ids := strings.Split(strings.TrimPrefix(line, pre), " -> ")
	ring := ids[:len(ids)-1]
	m := 0
	for i := range ring {
		if ring[i] < ring[m] {
			m = i
		}
	}
	rot := append(append([]string(nil), ring[m:]...), ring[:m]...)
	return pre + strings.Join(append(rot, rot[0]), " -> ")
}

func canonStderr(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = canonCycle(l)
	}
	return strings.Join(lines, "\n")
}

// pyException returns the exception type of a traceback golden, or "".
func pyException(golden string) string {
	line := strings.TrimSuffix(golden, "\n")
	if strings.Contains(line, "\n") || strings.HasPrefix(line, "decisions: ") {
		return ""
	}
	typ, _, ok := strings.Cut(line, ": ")
	if !ok || strings.ContainsAny(typ, " '") {
		return ""
	}
	return typ[strings.LastIndex(typ, ".")+1:]
}

func TestGoldenParity(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "perl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("could not run: %s is not on PATH (scenarios.sh needs bash, jq and perl)", tool)
		}
	}
	here, _ := os.Getwd()
	work := t.TempDir()
	out, err := exec.Command("bash", filepath.Join(here, "testdata", "scenarios.sh"), work).Output()
	if err != nil {
		t.Fatalf("scenarios.sh: %v", err)
	}
	t.Chdir(work)
	golden := filepath.Join(here, "testdata", "golden")
	cases, crashes, nonzero := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, argstr, _ := strings.Cut(line, "\t")
		cases++
		t.Run(name, func(t *testing.T) {
			read := func(ext string) string {
				b, err := os.ReadFile(filepath.Join(golden, name+ext))
				if err != nil {
					t.Fatalf("golden: %v", err)
				}
				return string(b)
			}
			wantOut, wantErr := read(".stdout"), read(".stderr")
			wantCode, _ := strconv.Atoi(strings.TrimSpace(read(".exit")))
			if err := os.Chdir(filepath.Join(work, name)); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Chdir(work) }()
			var stdout, stderr bytes.Buffer
			code := run(strings.Fields(argstr), &stdout, &stderr)
			if wantCode != 0 {
				nonzero++
			}
			if code != wantCode {
				t.Errorf("exit %d, golden %d\nstderr:\n%s", code, wantCode, stderr.String())
			}
			if stdout.String() != wantOut {
				t.Errorf("stdout differs\n got: %q\nwant: %q", stdout.String(), wantOut)
			}
			if typ := pyException(wantErr); typ != "" {
				crashes++
				if !strings.HasPrefix(stderr.String(), "decisions: crash: "+typ+": ") {
					t.Errorf("check.py raised %s; stderr: %q", typ, stderr.String())
				}
			} else {
				want := strings.ReplaceAll(wantErr, "run check.py --sync-edges", "run "+syncEdgesHint)
				if got := canonStderr(stderr.String()); got != canonStderr(want) {
					t.Errorf("stderr differs\n got: %q\nwant: %q", got, want)
				}
			}
			if wantSync, err := os.ReadFile(filepath.Join(golden, name+".sync.json")); err == nil {
				got, _ := os.ReadFile("sync.json")
				if !bytes.Equal(got, wantSync) {
					t.Errorf("sync.json differs (%d bytes, golden %d)", len(got), len(wantSync))
				}
			}
		})
	}
	if cases < 200 || crashes < 40 || nonzero < 180 {
		t.Fatalf("too few cases replayed: %d cases, %d crashes, %d non-zero exits", cases, crashes, nonzero)
	}
	t.Logf("%d cases replayed (%d crash cases, %d non-zero exits)", cases, crashes, nonzero)
}

func TestUnicodeTables(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "py39-unicode.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sets := map[string]map[rune]bool{}
	folds := map[rune][]rune{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) == 0 || strings.HasPrefix(fs[0], "#") {
			continue
		}
		if fs[0] == "fold" {
			for _, h := range fs[2:] {
				v, _ := strconv.ParseInt(h, 16, 32)
				folds[rune(fs[1][0])] = append(folds[rune(fs[1][0])], rune(v))
			}
			continue
		}
		a, _ := strconv.ParseInt(fs[1], 16, 32)
		b, _ := strconv.ParseInt(fs[2], 16, 32)
		if sets[fs[0]] == nil {
			sets[fs[0]] = map[rune]bool{}
		}
		for r := rune(a); r <= rune(b); r++ {
			sets[fs[0]][r] = true
		}
	}
	preds := map[string]func(rune) bool{"word": isWord, "space": isSpace, "isspace": isSpace, "decimal": isDecimal, "printable": isPrintable}
	assigned := sets["assigned"]
	if len(assigned) < 140000 {
		t.Fatalf("assigned set too small: %d", len(assigned))
	}
	for name, pred := range preds {
		bad := 0
		for r := rune(0); r < 0x110000; r++ {
			if !assigned[r] {
				continue
			}
			if pred(r) != sets[name][r] {
				if bad++; bad <= 5 {
					t.Errorf("%s(U+%04X): go %v, python %v", name, r, pred(r), sets[name][r])
				}
			}
		}
	}
	if len(folds) != 15 {
		t.Fatalf("fold letters: %d", len(folds))
	}
	for want, matches := range folds {
		m := map[rune]bool{}
		for _, r := range matches {
			m[r] = true
		}
		for r := rune(0); r < 0x110000; r++ {
			if !isSurrogate(r) && fold(r, want) != m[r] {
				t.Errorf("fold(U+%04X, %c): go %v, python %v", r, want, fold(r, want), m[r])
			}
		}
	}
}
