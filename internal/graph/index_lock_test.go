package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// The immediate write transaction can block behind another process's index.
// A lock timeout must be reported as contention rather than an opaque error.
func TestAnalyzeRepoReportsIndexInProgress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package p\nfunc Keep(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "graph.db")
	holder, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	other, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = AnalyzeRepoContext(ctx, dir, other)
	if err == nil || !strings.Contains(err.Error(), "index in progress") {
		t.Fatalf("lock contention was not explained: %v", err)
	}
}
