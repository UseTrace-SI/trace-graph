package graph

import (
	"os"
	"path/filepath"
	"strings"
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

// C and C++ headers and .cxx sources, and .mjs/.cjs modules are sources in
// every build; YAML is configuration and is never indexed.
func TestHeaderModuleAndConfigurationExtensions(t *testing.T) {
	dir := t.TempDir()
	// Default builds inventory C/C++ types; Tree-sitter builds declare
	// functions. Every source must yield a symbol either way.
	sources := map[string]string{
		"api.h":       "struct CHeader { int x; };\nvoid c_header(void) {}\n",
		"widget.hpp":  "class Widget {};\nvoid hpp_func() {}\n",
		"util.hh":     "struct HH {};\nvoid hh_func() {}\n",
		"inline.hxx":  "struct HXX {};\nvoid hxx_func() {}\n",
		"impl.cxx":    "struct CXX {};\nvoid cxx_func() {}\n",
		"mod.mjs":     "export function mjsFunc() {}\n",
		"common.cjs":  "function cjsFunc() {}\nmodule.exports = { cjsFunc }\n",
		"deploy.yaml": "class: Fake\ntype: Config\nfunction: notCode\n",
		"ci.yml":      "def: notCode\n",
	}
	for name, src := range sources {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(dir, db); err != nil {
		t.Fatal(err)
	}
	for name := range sources {
		if strings.HasSuffix(name, "ml") {
			continue
		}
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM symbols WHERE file=?", name).Scan(&count); err != nil || count == 0 {
			t.Errorf("%s not indexed: %d %v", name, count, err)
		}
	}
	var yaml int
	if err := db.QueryRow("SELECT COUNT(*) FROM file_cache_v2 WHERE path LIKE '%.y%ml'").Scan(&yaml); err != nil || yaml != 0 {
		t.Fatalf("YAML indexed: %d %v", yaml, err)
	}
	if InspectableSource("ci.yml") || !InspectableSource("include/api.h") {
		t.Fatal("inspection policy differs from indexing")
	}
}
