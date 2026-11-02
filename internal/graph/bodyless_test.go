//go:build !treesitter

package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestBodylessFunction(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.WriteFile(filepath.Join(dir, "extern.go"), []byte("package external\nfunc External()\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := AnalyzeRepo(dir, db); err != nil || n != 1 {
		t.Fatalf("bodyless declaration: %d %v", n, err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name='External'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("symbol missing: %d %v", n, err)
	}
}
