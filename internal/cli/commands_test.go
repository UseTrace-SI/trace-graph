package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymbolsQueryDefAndVersionCommands(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	src := "package p\nfunc Alpha(){}\ntype Kind struct{}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--repo", root, "--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, &out)
		}
		return out.String()
	}
	run("refresh")

	for _, want := range []string{"main.go:2  func  Alpha", "main.go:3  type  Kind"} {
		if got := run("symbols"); !strings.Contains(got, want) {
			t.Fatalf("symbols missing %q: %s", want, got)
		}
	}
	if got := run("query", "Alpha()"); !strings.Contains(got, "Alpha") {
		t.Fatalf("query missed symbol: %s", got)
	}
	// FTS operators are literal words, so this must not be a syntax error.
	if got := run("query", "AND", "OR"); strings.TrimSpace(got) != "" {
		t.Fatalf("literal query returned rows: %s", got)
	}
	if got := run("def", "Alpha"); !strings.Contains(got, "func Alpha") {
		t.Fatalf("def missing signature: %s", got)
	}
	if got := run("version"); !strings.HasPrefix(got, "trace-graph ") {
		t.Fatalf("unexpected version output: %s", got)
	}
	if got := run("capabilities"); !strings.Contains(got, `"analyzer"`) {
		t.Fatalf("capabilities missing analyzer: %s", got)
	}
}
