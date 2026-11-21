package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/UseTrace-SI/trace-graph/internal/store"
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
	wantSignature := "func Alpha"
	if strings.HasPrefix(graph.Capabilities().Analyzer, "treesitter") {
		wantSignature = "function_declaration Alpha"
	}
	if got := run("def", "Alpha"); !strings.Contains(got, wantSignature) {
		t.Fatalf("def missing signature %q: %s", wantSignature, got)
	}
	if got := run("version"); !strings.HasPrefix(got, "trace-graph ") {
		t.Fatalf("unexpected version output: %s", got)
	}
	if got := run("capabilities"); !strings.Contains(got, `"analyzer"`) {
		t.Fatalf("capabilities missing analyzer: %s", got)
	}
}

// symbols, query, neighbors and def opened the index through the migrating
// writer path. While a refresh held the SQLite writer they waited for the
// 30-second busy timeout and then failed with "database is locked"; health
// and impact already answered from the last committed generation.
func TestRegressionLookupsDoNotWaitForIndexWriter(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc Alpha(){}\nfunc Beta(){Alpha()}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	refresh := newRoot()
	refresh.SetOut(&bytes.Buffer{})
	refresh.SetArgs([]string{"--repo", root, "--db", path, "refresh"})
	if err := refresh.Execute(); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.Begin() // BEGIN IMMEDIATE: the same lock an index holds
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO repo_state(repo,indexed_at) VALUES('in-progress',1)"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"symbols"}, "main.go:2  func  Alpha"},
		{[]string{"query", "Alpha"}, "Alpha"},
		{[]string{"def", "Alpha"}, "main.go:2"},
		{[]string{"neighbors", "Alpha"}, "main.go:2  func"}, // Tree-sitter builds leave callers unresolved
	} {
		done := make(chan struct{})
		var out bytes.Buffer
		var runErr error
		go func() {
			defer close(done)
			cmd := newRoot()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append([]string{"--repo", root, "--db", path}, tc.args...))
			runErr = cmd.Execute()
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			tx.Rollback() // release the writer so the command can finish
			<-done
			t.Fatalf("%v waited behind the index writer", tc.args)
		}
		if runErr != nil || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%v: %v %q, want %q", tc.args, runErr, &out, tc.want)
		}
	}
}
