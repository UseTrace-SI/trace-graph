//go:build treesitter

package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// Any node kind containing "function" or "method" used to become a
// declaration named after its first identifier-like child. Call sites
// (method_invocation, generic_function), types (function_type) and
// parameters (arrow functions, Ruby method_parameters and C return types)
// became fake symbols, and same-name ones on one line collided, failing the
// whole index. Java calls were never recorded because extraction stopped at
// every invocation.
func TestRegressionTreeSitterCallSitesAndTypesAreNotDeclarations(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Builder.java": "class Builder {\n  String build(StringBuilder sb, String a, String b) {\n    return sb.append(a).append(b).toString();\n  }\n}\n",
		"chain.js":     "function chain(xs) {\n  return xs.map(x => x).filter(x => x);\n}\n",
		"types.ts":     "function apply(f: () => Result, g: () => Result): void {}\n",
		"conv.rs":      "fn conv(a: &str, b: &str) -> u32 {\n  into::<u32>(a) + into::<u32>(b)\n}\n",
		"greet.rb":     "def greet(name, punctuation)\n  name\nend\n",
		"make.c":       "typedef int Foo;\nFoo make(void) { return 0; }\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatalf("call sites collided as declarations: %v", err)
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, want := range map[string]int{
		"Builder": 1, "build": 1, "chain": 1, "apply": 1, "conv": 1, "greet": 1, "make": 1,
		"append": 0, "toString": 0, "x": 0, "Result": 0, "into": 0, "name": 0, "Foo": 0,
	} {
		if got := count("SELECT count(*) FROM symbols WHERE repo=? AND name=?", root, name); got != want {
			t.Errorf("%s: %d symbols, want %d", name, got, want)
		}
	}
	if got := count("SELECT count(*) FROM relations WHERE repo=? AND from_symbol='build' AND to_symbol IN ('append','toString')", root); got < 2 {
		t.Errorf("Java invocations were not observed as calls: %d", got)
	}
}
