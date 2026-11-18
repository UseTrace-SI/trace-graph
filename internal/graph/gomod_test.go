package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// go.mod is not a source file, but its module path changes how imports map to
// package namespaces. A module-only edit must invalidate every cached file.
func TestGoModChangeInvalidatesSourceCache(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/old\ngo 1.26\n")
	write("a/a.go", "package a\nimport b \"example.test/old/b\"\nfunc Caller(){b.Target()}\n")
	write("b/b.go", "package b\nfunc Target(){}\n")

	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	first, err := AnalyzeRepo(root, db)
	if err != nil || first == 0 {
		t.Fatalf("initial analysis: %d %v", first, err)
	}
	resolved := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE to_id IS NOT NULL AND resolution='lexical'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	initial := resolved()

	// Only go.mod changes: source bytes, sizes and timestamps stay identical.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/new\ngo 1.26\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := AnalyzeRepo(root, db)
	if err != nil || second == 0 {
		t.Fatalf("go.mod-only change kept cache hits: %d %v", second, err)
	}
	if !strings.HasPrefix(Capabilities().Analyzer, "treesitter") && initial == 1 && resolved() != 0 {
		t.Fatal("stale module-local call edge after go.mod change")
	}
}
