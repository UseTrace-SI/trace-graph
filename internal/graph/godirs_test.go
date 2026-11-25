package graph

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// An ignored Go file beside indexed ones makes its package incomplete for
// the type pass. Adding or removing it changed typed edges without changing
// any indexed source, and the skipped resolution kept stale typed edges.
func TestRegressionIgnoredGoFileChangesTypedResolution(t *testing.T) {
	if !goTypesEnabled {
		t.Skip("no Go type pass in this build")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module m\n")
	write(".gitignore", "a/hidden_gen.go\n")
	write("a/a.go", "package a\ntype Inner struct{}\nfunc (Inner) M() {}\ntype Outer struct{ Inner }\n")
	write("main.go", "package main\nimport \"m/a\"\nfunc Use() { var o a.Outer; o.M() }\n")

	resolution := func(db *sql.DB) string {
		t.Helper()
		var res string
		if err := db.QueryRow("SELECT t.resolution FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.repo=? AND r.from_symbol='Use' AND r.to_symbol='M'", root).Scan(&res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	fresh := func() string {
		t.Helper()
		db, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := AnalyzeRepo(root, db); err != nil {
			t.Fatal(err)
		}
		return resolution(db)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	refresh := func() string {
		t.Helper()
		if _, err := AnalyzeRepo(root, db); err != nil {
			t.Fatal(err)
		}
		return resolution(db)
	}
	if got, want := refresh(), fresh(); got != "typed" || got != want {
		t.Fatalf("initial: incremental %q, fresh %q", got, want)
	}
	write("a/hidden_gen.go", "package a\nfunc (Outer) M() {}\n")
	if got, want := refresh(), fresh(); got == "typed" || got != want {
		t.Fatalf("ignored file added: incremental %q, fresh %q", got, want)
	}
	if err := os.Remove(filepath.Join(root, "a", "hidden_gen.go")); err != nil {
		t.Fatal(err)
	}
	if got, want := refresh(), fresh(); got != "typed" || got != want {
		t.Fatalf("ignored file removed: incremental %q, fresh %q", got, want)
	}

	// A binary that does not record the directory fingerprint resolved
	// while the directory looked different (here: while the ignored file
	// existed) and overwrote the pass record. The next refresh cannot trust
	// that pass, even though nothing changed since.
	if _, err := db.Exec("UPDATE relation_targets SET to_id=NULL,resolution='unresolved' WHERE relation_id IN (SELECT id FROM relations WHERE repo=? AND to_symbol='M')", root); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE repo_resolution SET detail='{"status":"complete"}' WHERE repo=?`, root); err != nil {
		t.Fatal(err)
	}
	if got, want := refresh(), fresh(); got != "typed" || got != want {
		t.Fatalf("pass written by another binary was trusted: incremental %q, fresh %q", got, want)
	}
}
