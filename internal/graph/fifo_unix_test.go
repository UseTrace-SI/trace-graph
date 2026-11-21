//go:build unix

package graph

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// A FIFO named like a source is skipped; opening it would block the
// deadline-bounded index indefinitely.
func TestIndexSkipsFIFOSources(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	revisionTestWrite(t, root, "real.go", "package p\nfunc Real(){}\n")
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.go"), 0600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	done := make(chan error, 1)
	go func() {
		_, err := AnalyzeRepo(root, db)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("indexing blocked on a FIFO")
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=? AND name='Real'", root).Scan(&n); err != nil || n != 1 {
		t.Fatalf("regular source missing: %d %v", n, err)
	}
}
