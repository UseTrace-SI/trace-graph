package graph

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestRegressionIndexMutationsAndIsolation(t *testing.T) {
	base := t.TempDir()
	db, err := store.Open(filepath.Join(base, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, src string) {
		t.Helper()
		path := filepath.Join(dir, "main.go")
		if err := os.WriteFile(path, []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		// Identical timestamps and sizes must not hide a content change.
		stamp := time.Unix(1700000000, 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	index := func(dir string) {
		t.Helper()
		if _, err := AnalyzeRepo(dir, db); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	write(a, "package main\nfunc Old() { Old() }\n")
	index(a)
	write(a, "package main\nfunc New() { New() }\n")
	index(a)
	if count("SELECT count(*) FROM symbols WHERE name='Old'") != 0 || count("SELECT count(*) FROM relations WHERE to_symbol='Old'") != 0 {
		t.Fatal("stale symbols or edges")
	}
	if count("SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH 'Old'") != 0 {
		t.Fatal("stale search index")
	}
	if count("SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH 'New'") != 1 {
		t.Fatal("new symbol missing from search")
	}
	write(b, "package main\nfunc New() { New() }\n")
	index(b)
	if count("SELECT count(*) FROM symbols WHERE name='New'") != 2 {
		t.Fatal("cross-repository collision")
	}
	if err := os.Remove(filepath.Join(a, "main.go")); err != nil {
		t.Fatal(err)
	}
	index(a)
	if count("SELECT count(*) FROM symbols WHERE repo=?", a) != 0 || count("SELECT count(*) FROM relations WHERE repo=?", a) != 0 {
		t.Fatal("deleted file remains indexed")
	}
	if count("SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH 'New'") != 1 {
		t.Fatal("deletion damaged another repo's search")
	}
	if n, err := AnalyzeRepo(b, db); err != nil || n != 0 {
		t.Fatalf("unchanged cache: %d %v", n, err)
	}
}

func TestRegressionIndexRollsBackDatabaseFailure(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\nfunc Old() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(dir, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER reject_new BEFORE INSERT ON symbols BEGIN SELECT RAISE(ABORT,'injected write failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package main\nfunc New() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(dir, db); err == nil {
		t.Fatal("write failure swallowed")
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name='Old'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("old index lost: %d %v", n, err)
	}
}
