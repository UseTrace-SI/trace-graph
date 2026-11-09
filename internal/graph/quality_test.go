//go:build !treesitter

package graph

import (
	"github.com/UseTrace-SI/trace-graph/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestQualifiedCallsDoNotMixPackagesOrMethods(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.test/repo\ngo 1.26\n",
		"a/a.go": "package a\nimport b \"example.test/repo/b\"\nfunc Same() {}\nfunc Caller() { Same(); b.Same() }\ntype T struct{}\nfunc (T) Same() {}\nfunc Member(t T) { t.Same() }\n",
		"b/b.go": "package b\nfunc Same() {}\nfunc Caller() { Same() }\n",
	}
	for file, src := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var resolved, unresolved int
	if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE resolution='lexical'").Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE resolution='unresolved' AND to_id IS NULL").Scan(&unresolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 4 || unresolved != 0 {
		t.Fatalf("incorrect resolution: resolved=%d unresolved=%d", resolved, unresolved)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM relations r JOIN relation_targets t ON t.relation_id=r.id JOIN symbols s ON s.id=t.to_id WHERE r.file='b/b.go' AND s.file='a/a.go'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cross-package false edge: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE kind='method' AND signature LIKE '%(T)%'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("receiver signature lost: %d %v", n, err)
	}
	// An incremental deletion must remove previously resolved incoming targets.
	if err := os.WriteFile(filepath.Join(root, "b/b.go"), []byte("package b\nfunc Other() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE to_id IS NOT NULL AND to_id NOT IN (SELECT id FROM symbols)").Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale target: %d %v", n, err)
	}
}
func TestInventoryIgnoresCommentAndStringDeclarations(t *testing.T) {
	root := t.TempDir()
	src := "/*\nfunction FakeComment() {}\n*/\nconst text = `\nfunction FakeString() {}\n`;\nexport const realArrow = async (x: number) => x;\nexport default function RealFunction() {}\n"
	if err := os.WriteFile(filepath.Join(root, "index.mts"), []byte(src), 0600); err != nil {
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
	for _, tc := range []struct {
		name  string
		count int
	}{{"FakeComment", 0}, {"FakeString", 0}, {"realArrow", 1}, {"RealFunction", 1}} {
		var got int
		if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name=?", tc.name).Scan(&got); err != nil || got != tc.count {
			t.Fatalf("%s: %d %v", tc.name, got, err)
		}
	}
}
