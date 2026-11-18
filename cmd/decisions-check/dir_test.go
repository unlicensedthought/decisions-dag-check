package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleDir copies the sample decisions layout so a test can break it.
func sampleDir(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "decisions")
	dst := t.TempDir()
	for _, sub := range []string{"nodes", "ledger"} {
		if err := os.Mkdir(filepath.Join(dst, sub), 0o700); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(src, sub, "*"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, sub, filepath.Base(f)), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(src, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "meta.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func runDir(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	rc := run(args, &o, &e)
	return rc, o.String(), e.String()
}

func TestDirLayoutPassesOnTheSampleTree(t *testing.T) {
	rc, out, errs := runDir("--dir", sampleDir(t))
	if rc != 0 || !strings.HasPrefix(out, "decisions: ok (") {
		t.Fatalf("rc=%d out=%q err=%q", rc, out, errs)
	}
}

func TestDirLayoutNegatives(t *testing.T) {
	node := func(d, id string) string { return filepath.Join(d, "nodes", id+".json") }
	const id = "D-kick"
	cases := []struct {
		name string
		edit func(t *testing.T, d string)
		want string
	}{
		{"malformed node file", func(t *testing.T, d string) {
			os.WriteFile(node(d, id), []byte("{\"id\": "), 0o600)
		}, "nodes/" + id + ".json: "},
		{"node that is not an object", func(t *testing.T, d string) {
			os.WriteFile(node(d, id), []byte("[]"), 0o600)
		}, "want a JSON object"},
		{"file name says another id", func(t *testing.T, d string) {
			b, _ := os.ReadFile(node(d, id))
			os.WriteFile(node(d, "D-other"), b, 0o600)
		}, `the file name says "D-other"`},
		{"stray non-json file in nodes", func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "nodes", "notes.txt"), []byte("x"), 0o600)
		}, "not a <id>.json file"},
		{"node removed", func(t *testing.T, d string) {
			os.Remove(node(d, id))
		}, "ledger: " + id + " was removed from the DAG"},
		{"node without a ledger file", func(t *testing.T, d string) {
			os.Remove(filepath.Join(d, "ledger", id+".tsv"))
		}, "ledger: " + id + " is not in the ledger"},
		{"node edited without a corrected line", func(t *testing.T, d string) {
			b, _ := os.ReadFile(node(d, id))
			os.WriteFile(node(d, id), bytes.Replace(b, []byte(`"title": "`), []byte(`"title": "X`), 1), 0o600)
		}, "ledger: " + id + " changed its title/decision/decided_by/source"},
		{"ledger line under the wrong file", func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "ledger", "D-stray.tsv"), []byte(id+"\tabcdef012345\tadded\n"), 0o600)
		}, "belongs in ledger/" + id + ".tsv"},
		{"malformed ledger line", func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "ledger", id+".tsv"), []byte("garbage\n"), 0o600)
		}, "want id<TAB>fingerprint<TAB>note"},
		{"edges stored in meta.json", func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "meta.json"), []byte(`{"nodes":[],"topics":{}}`), 0o600)
		}, "live in nodes/<id>.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := sampleDir(t)
			c.edit(t, d)
			rc, _, errs := runDir("--dir", d)
			if rc != 1 || !strings.Contains(errs, c.want) {
				t.Fatalf("rc=%d, want exit 1 with %q\nstderr: %s", rc, c.want, errs)
			}
		})
	}
}

func TestDirLayoutCorrectedLineAllowsAnEdit(t *testing.T) {
	d := sampleDir(t)
	const id = "D-kick"
	p := filepath.Join(d, "nodes", id+".json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(b, []byte(`"title": "`), []byte(`"title": "X`), 1), 0o600)
	_, fp, _ := runDir("--fingerprint", "--dir", d)
	var line string
	for _, l := range strings.Split(fp, "\n") {
		if strings.HasPrefix(l, id+"\t") {
			line = l
		}
	}
	lp := filepath.Join(d, "ledger", id+".tsv")
	lb, _ := os.ReadFile(lp)
	os.WriteFile(lp, append(lb, []byte(line+"\tcorrected: test\n")...), 0o600)
	if rc, _, errs := runDir("--dir", d); rc != 0 {
		t.Fatalf("rc=%d %s", rc, errs)
	}
}

func TestDirLayoutRefusesWhatItHasNoUseFor(t *testing.T) {
	d := sampleDir(t)
	for _, args := range [][]string{{"--sync-edges", "--dir", d}, {"--dir", d, "--ledger", "x"}, {"--dir", d, "extra.json"}} {
		if rc, _, errs := runDir(args...); rc != 1 || errs == "" {
			t.Fatalf("%v: rc=%d err=%q", args, rc, errs)
		}
	}
}
