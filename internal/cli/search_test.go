package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// query used to match whole FTS tokens only, so "analyze" missed
// AnalyzeRepo, AnalyzeRepoContext and analyzeFile.
func TestQueryMatchesPartialIdentifierNames(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	src := "package p\nfunc AnalyzeRepo(){}\nfunc AnalyzeRepoContext(){}\nfunc analyzeFile(){}\nfunc Analyze(){}\nfunc Unrelated(){}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tool.py"), []byte("def parse_http_request():\n    pass\n"), 0600); err != nil {
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
	got := strings.Split(strings.TrimSpace(run("query", "analyze")), "\n")
	if len(got) != 4 || !strings.HasSuffix(got[0], "  Analyze") {
		t.Fatalf("query analyze: exact name not first or matches missing: %q", got)
	}
	for _, want := range []string{"AnalyzeRepo", "AnalyzeRepoContext", "analyzeFile"} {
		if !strings.Contains(strings.Join(got, "\n"), "  "+want+"\n") && !strings.HasSuffix(strings.Join(got, "\n"), "  "+want) {
			t.Fatalf("query analyze missed %s: %q", want, got)
		}
	}
	if got := run("query", "context"); !strings.Contains(got, "AnalyzeRepoContext") {
		t.Fatalf("split-word match missing: %s", got)
	}
	if got := run("query", "http"); !strings.Contains(got, "parse_http_request") {
		t.Fatalf("snake_case word match missing: %s", got)
	}
}

// context --semantic used to rank only the first 2,000 symbols in file
// order and ignore the query words, so a match in a later file was never
// considered. It now reranks search matches and path matches.
func TestContextSemanticPrefiltersBySearchAndPath(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	var filler strings.Builder
	filler.WriteString("package a\n")
	for i := 0; i < 2500; i++ {
		fmt.Fprintf(&filler, "func Filler%04d(){}\n", i)
	}
	write := func(rel, src string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a/a.go", filler.String())
	write("z/config.go", "package z\nfunc ParseConfigFile(){}\nfunc Other(){}\n")
	write("router/table.go", "package router\nfunc Lookup(){}\n")
	runContextCLI(t, root, path, "refresh")

	semantic := func(args ...string) []codeSymbol {
		t.Helper()
		var got codeContext
		out := runContextCLI(t, root, path, append([]string{"context", "--semantic"}, args...)...)
		if err := json.Unmarshal(out, &got); err != nil || got.Status != "ready" {
			t.Fatalf("%+v %v %s", got, err, out)
		}
		return got.Symbols
	}
	if got := semantic("--query", "parse config"); len(got) == 0 || got[0].Name != "ParseConfigFile" {
		t.Fatalf("semantic search missed a symbol after the first 2,000: %+v", got)
	}
	got := semantic("--query", "router")
	if len(got) != 1 || got[0].Name != "Lookup" {
		t.Fatalf("path words were not candidates: %+v", got)
	}
	if got := semantic("--query", "config", "--path", "router/table.go"); len(got) != 0 {
		t.Fatalf("--path was ignored: %+v", got)
	}
	if got := semantic("--query", "unmatched words"); len(got) != 0 {
		t.Fatalf("query words were ignored: %+v", got)
	}
}
