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
// one minified file block every other source. The first declaration is
// kept, later ones are dropped with their observations and a recorded
// warning, and the file is analyzed again once it changes.
func TestAnalyzeRepoDropsCollidingDeclarationsWithWarning(t *testing.T) {
	// AnalyzeRepoContext stores the canonical repository identity. On macOS
	// t.TempDir() lives under /var, a symlink to /private/var, so queries must
	// use the resolved path or they silently match no rows.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "p.go")
	valid := "package p\ntype T struct{}\ntype U struct{}\nfunc (T) Good() {}\n"
	colliding := "package p\ntype T struct{}\ntype U struct{}\nfunc(T)Same(){ first() };func(U)Same(){ second() }\nfunc After() {}\n"
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

	if err := os.WriteFile(source, []byte(colliding), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := AnalyzeRepoReport(context.Background(), root, db)
	if err != nil {
		t.Fatalf("one colliding file failed the whole index: %v", err)
	}
	// The warning names the colliding declaration so it can be found.
	if len(report.Warnings) != 1 || report.Warnings[0].Path != "p.go" || !strings.Contains(report.Warnings[0].Warning, "colliding declaration identities (p.go:Same:4)") {
		t.Fatalf("collision not reported: %+v", report.Warnings)
	}
	// T, U, the first Same and After remain; the second Same is dropped.
	if n := count("SELECT count(*) FROM symbols WHERE repo=? AND file='p.go'"); n != 4 || report.Symbols != 4 {
		t.Fatalf("kept %d symbols (report %d), want 4", n, report.Symbols)
	}
	if n := count("SELECT count(*) FROM symbols WHERE repo=? AND name='After'"); n != 1 {
		t.Fatal("declarations after the collision were lost")
	}
	// The kept Same keeps its own receiver and call, never the dropped one's.
	// (Tree-sitter builds record no receivers.)
	if strings.HasPrefix(analyzerVersion, "inventory") {
		var receiver string
		if err := db.QueryRow("SELECT r.receiver FROM symbol_receivers r JOIN symbols s ON s.id=r.symbol_id WHERE s.repo=? AND s.name='Same'", root).Scan(&receiver); err != nil || receiver != "T" {
			t.Fatalf("receiver %q %v, want T", receiver, err)
		}
	}
	if count("SELECT count(*) FROM relations WHERE repo=? AND from_symbol='Same' AND to_symbol='first'") != 1 || count("SELECT count(*) FROM relations WHERE repo=? AND to_symbol='second'") != 0 {
		t.Fatal("dropped declaration's calls were attributed to the kept one")
	}
	if n := count("SELECT count(*) FROM symbols WHERE repo=? AND file='other.go'"); n != 1 {
		t.Fatalf("other sources were not indexed: %d", n)
	}

	// An unchanged refresh keeps the warning without re-analysis.
	report, err = AnalyzeRepoReport(context.Background(), root, db)
	if err != nil || report.Symbols != 0 || len(report.Warnings) != 1 {
		t.Fatalf("unchanged refresh: %+v %v", report, err)
	}

	if err := os.WriteFile(source, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = AnalyzeRepoReport(context.Background(), root, db)
	if err != nil || len(report.Warnings) != 0 {
		t.Fatalf("fixed file still warned: %+v %v", report, err)
	}
	if count("SELECT count(*) FROM symbols WHERE repo=? AND file='p.go' AND name='Good'") != 1 {
		t.Fatal("fixed file was not analyzed again")
	}

	// Deleting a warned file removes its warning.
	if err := os.WriteFile(source, []byte(colliding), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if report, err := AnalyzeRepoReport(context.Background(), root, db); err != nil || len(report.Warnings) != 0 {
		t.Fatalf("deleted file still reported: %+v %v", report, err)
	}
}

// A one-line TypeScript overload signature and its implementation: the
// symbol keeps the implementation and its calls in every build.
func TestOneLineOverloadKeepsImplementation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "export function f(a: string): void; export function f(a: any) { g(a) }\nfunction g(x: any) {}\n"
	if err := os.WriteFile(filepath.Join(root, "a.ts"), []byte(src), 0600); err != nil {
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
	var signature string
	if err := db.QueryRow("SELECT signature FROM symbols WHERE repo=? AND name='f'", root).Scan(&signature); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(signature, "function_signature") {
		t.Fatalf("bodyless overload kept: %q", signature)
	}
	var calls int
	if err := db.QueryRow("SELECT count(*) FROM relations WHERE repo=? AND from_symbol='f' AND to_symbol='g'", root).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("implementation's call lost: %d %v", calls, err)
	}
}
