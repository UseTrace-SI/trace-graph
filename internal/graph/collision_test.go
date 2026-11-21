package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// Persistent AnalyzeRepo used to gate its collision guard on maxRecords, so a
// second declaration sharing the same file/name/line silently replaced the
// first, losing its signature and receiver and creating incorrect edges.
func TestAnalyzeRepoRejectsCollidingDeclarationIdentities(t *testing.T) {
	// AnalyzeRepoContext stores the canonical repository identity. On macOS
	// t.TempDir() lives under /var, a symlink to /private/var, so queries must
	// use the resolved path or they silently match no rows.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "p.go")
	valid := "package p\ntype T struct{}\ntype U struct{}\nfunc (T) Good() {}\n"
	colliding := "package p\ntype T struct{}\ntype U struct{}\nfunc(T)Same(){};func(U)Same(){}\n"
	if err := os.WriteFile(source, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var before, receiversBefore int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=?", root).Scan(&before); err != nil || before == 0 {
		t.Fatalf("initial generation: %d %v", before, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM symbol_receivers").Scan(&receiversBefore); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(source, []byte(colliding), 0600); err != nil {
		t.Fatal(err)
	}
	// The error names the colliding declaration so it can be found.
	if _, err := AnalyzeRepo(root, db); err == nil || !strings.Contains(err.Error(), "colliding declaration identities (p.go:Same:4)") {
		t.Fatalf("persistent analysis accepted or did not locate a collision: %v", err)
	}
	var symbols, files, receivers int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=?", root).Scan(&symbols); err != nil || symbols != before {
		t.Fatalf("collision damaged the prior generation: %d != %d %v", symbols, before, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM file_cache_v2 WHERE repo=?", root).Scan(&files); err != nil || files != 1 {
		t.Fatalf("collision committed a partial generation: %d %v", files, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM symbol_receivers").Scan(&receivers); err != nil || receivers != receiversBefore {
		t.Fatalf("collision wrote a receiver: %d != %d %v", receivers, receiversBefore, err)
	}
}
