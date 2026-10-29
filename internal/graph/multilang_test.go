package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestAnalyzeMultiLangAndQuery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc Foo() {}\ntype Bar struct{}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("def hello():\n    pass\nclass World:\n    pass\n"), 0644)
	db, err := store.Open(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := AnalyzeRepo(dir, db)
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Fatalf("expected >=4 symbols, got %d", n)
	}
	var count int
	db.QueryRow("SELECT COUNT(*) FROM symbols WHERE name='hello'").Scan(&count)
	if count != 1 {
		t.Fatalf("hello not found")
	}
	db.QueryRow("SELECT COUNT(*) FROM symbols WHERE name='Foo'").Scan(&count)
	if count != 1 {
		t.Fatalf("Foo not found")
	}
}
