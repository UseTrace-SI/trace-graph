package cli

import (
	"bytes"
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
