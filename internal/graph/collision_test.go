package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// Two same-name declarations on one line share a file/name/line identity.
// Storing both would silently replace the first (losing its signature and
// receiver and creating incorrect edges), and failing the whole index made
// one minified file block every other source. The file is skipped with a
// recorded warning instead, and analyzed again once it changes.
func TestAnalyzeRepoSkipsFileWithCollidingDeclarationIdentities(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(root, "other.go"), []byte("package p\nfunc Other() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(query string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(query, root).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	if count("SELECT count(*) FROM symbols WHERE repo=? AND file='p.go'") == 0 {
		t.Fatal("initial generation has no symbols")
	}

	if err := os.WriteFile(source, []byte(colliding), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := AnalyzeRepoReport(context.Background(), root, db)
	if err != nil {
		t.Fatalf("one colliding file failed the whole index: %v", err)
	}
	// The warning names the colliding declaration so it can be found.
	if len(report.Skipped) != 1 || report.Skipped[0].Path != "p.go" || !strings.Contains(report.Skipped[0].Warning, "colliding declaration identities (p.go:Same:4)") {
		t.Fatalf("skipped file not reported: %+v", report.Skipped)
	}
	if n := count("SELECT count(*) FROM symbols WHERE repo=? AND file='p.go'"); n != 0 {
		t.Fatalf("skipped file kept %d stale or partial symbols", n)
	}
	if n := count("SELECT count(*) FROM symbol_receivers r JOIN symbols s ON s.id=r.symbol_id WHERE s.repo=?"); n != 0 {
		t.Fatalf("skipped file wrote %d receivers", n)
	}
	if n := count("SELECT count(*) FROM symbols WHERE repo=? AND file='other.go'"); n != 1 {
		t.Fatalf("other sources were not indexed: %d", n)
	}

	// An unchanged refresh keeps the warning without re-analysis.
	report, err = AnalyzeRepoReport(context.Background(), root, db)
	if err != nil || report.Symbols != 0 || len(report.Skipped) != 1 {
		t.Fatalf("unchanged refresh: %+v %v", report, err)
	}

	if err := os.WriteFile(source, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = AnalyzeRepoReport(context.Background(), root, db)
	if err != nil || len(report.Skipped) != 0 {
		t.Fatalf("fixed file still skipped: %+v %v", report, err)
	}
	if count("SELECT count(*) FROM symbols WHERE repo=? AND file='p.go' AND name='Good'") != 1 {
		t.Fatal("fixed file was not analyzed again")
	}

	// Deleting a skipped file removes its warning.
	if err := os.WriteFile(source, []byte(colliding), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if report, err := AnalyzeRepoReport(context.Background(), root, db); err != nil || len(report.Skipped) != 0 {
		t.Fatalf("deleted file still reported: %+v %v", report, err)
	}
}
