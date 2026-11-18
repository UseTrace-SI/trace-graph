package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// A repository is identified by its canonical path. Indexing through a
// symlink must scope symbols and the file cache to the resolved path, which is
// what makes macOS (/var -> /private/var) behave like Linux.
func TestAnalyzeRepoUsesCanonicalRepoIdentity(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "p.go"), []byte("package p\nfunc SymlinkProbe(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked-repo")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if canonical == link {
		t.Fatal("fixture did not create a symlinked repository path")
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	first, err := AnalyzeRepo(link, db)
	if err != nil || first == 0 {
		t.Fatalf("symlinked repository was not indexed: %d %v", first, err)
	}
	var stored int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=?", canonical).Scan(&stored); err != nil || stored == 0 {
		t.Fatalf("symlinked repository indexed under non-canonical identity: %d %v", stored, err)
	}
	// Both spellings must resolve to the same cached generation.
	for _, spelling := range []string{canonical, link} {
		n, err := AnalyzeRepo(spelling, db)
		if err != nil || n != 0 {
			t.Fatalf("reindex via %s: %d symbols, %v", spelling, n, err)
		}
	}
}
