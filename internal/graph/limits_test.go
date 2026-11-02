package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestBoundedIndexKeepsPriorGeneration(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc Keep(){}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = AnalyzeRepo(dir, db); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "large.go"))
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(8*1024*1024 + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AnalyzeRepo(dir, db); err == nil {
		t.Fatal("oversized source accepted")
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM symbols WHERE name='Keep'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("prior generation damaged: %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = AnalyzeRepoContext(ctx, dir, db); err == nil {
		t.Fatal("cancelled index succeeded")
	}
}
