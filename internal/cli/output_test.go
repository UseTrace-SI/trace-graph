package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
)

var repoHashPrefix = regexp.MustCompile(`[0-9a-f]{64}:`)

func TestSymbolsAndQueryLimitAndJSON(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc AnalyzeRepo(){}\nfunc AnalyzeFile(){}\nfunc Other(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runContextCLI(t, root, path, "refresh")
	if got := strings.Count(string(runContextCLI(t, root, path, "symbols", "--limit", "2")), "\n"); got != 2 {
		t.Fatalf("symbols --limit 2 printed %d lines", got)
	}
	if got := strings.Count(string(runContextCLI(t, root, path, "symbols")), "\n"); got != 3 {
		t.Fatalf("symbols printed %d lines", got)
	}
	var list symbolList
	out := runContextCLI(t, root, path, "query", "analyze", "--limit", "1", "--json")
	if err := json.Unmarshal(out, &list); err != nil || list.Protocol != 1 || list.Repo != root || len(list.Symbols) != 1 || !list.Truncated {
		t.Fatalf("query --json --limit: %+v %v %s", list, err, out)
	}
	if list.Symbols[0].ID != "main.go:"+list.Symbols[0].Name+":"+map[string]string{"AnalyzeRepo": "2", "AnalyzeFile": "3"}[list.Symbols[0].Name] || list.Symbols[0].Signature == "" {
		t.Fatalf("query --json symbol: %+v", list.Symbols[0])
	}
	out = runContextCLI(t, root, path, "symbols", "--json")
	if err := json.Unmarshal(out, &list); err != nil || len(list.Symbols) != 3 || list.Truncated || repoHashPrefix.Match(out) {
		t.Fatalf("symbols --json: %+v %v %s", list, err, out)
	}
	if out := string(runContextCLI(t, root, path, "query", "nothing", "--json")); !strings.Contains(out, `"symbols":[]`) {
		t.Fatalf("empty query --json: %s", out)
	}
}

// Output scoped to one repository shows IDs without the 64-character
// repository hash, and every selector accepts the shown form.
func TestOutputIDsOmitRepositoryHash(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc Alpha(){}\nfunc Beta(){ Alpha() }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runContextCLI(t, root, path, "refresh")
	out := runContextCLI(t, root, path, "context", "--query", "alpha beta")
	var code codeContext
	if err := json.Unmarshal(out, &code); err != nil || len(code.Symbols) != 2 || repoHashPrefix.Match(out) {
		t.Fatalf("context IDs: %v %s", err, out)
	}
	if code.Symbols[0].ID != "main.go:Alpha:2" && code.Symbols[1].ID != "main.go:Alpha:2" {
		t.Fatalf("context IDs: %+v", code.Symbols)
	}
	resolved := !strings.HasPrefix(graph.Capabilities().Analyzer, "treesitter")
	if resolved && (len(code.Relations) == 0 || code.Relations[0].FromID != "main.go:Beta:3" || code.Relations[0].ToID != "main.go:Alpha:2") {
		t.Fatalf("context relation IDs: %+v", code.Relations)
	}
	for _, selector := range []string{"main.go:Alpha:2", graph.ScopedID(root, "main.go:Alpha:2")} {
		out := runContextCLI(t, root, path, "impact", selector, "--json")
		var report impactReport
		if err := json.Unmarshal(out, &report); err != nil || report.Symbol.ID != "main.go:Alpha:2" || repoHashPrefix.Match(out) {
			t.Fatalf("impact %s: %v %s", selector, err, out)
		}
		if resolved && (len(report.Nodes) != 1 || report.Nodes[0].ID != "main.go:Beta:3" || report.Nodes[0].Via != "main.go:Alpha:2") {
			t.Fatalf("impact nodes: %+v", report.Nodes)
		}
		if got := string(runContextCLI(t, root, path, "neighbors", selector)); !strings.Contains(got, "main.go:2  func") {
			t.Fatalf("neighbors %s: %s", selector, got)
		}
	}
	// The snapshot format keeps stored, repository-scoped IDs.
	if out := runContextCLI(t, root, path, "snapshot"); !repoHashPrefix.Match(out) {
		t.Fatalf("snapshot lost scoped IDs: %s", out)
	}
}

func TestUnscopedID(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for in, want := range map[string]string{
		hash + ":a.go:F:1":                  "a.go:F:1",
		"a.go:F:1":                          "a.go:F:1",
		strings.Repeat("g", 64) + ":a.go":   strings.Repeat("g", 64) + ":a.go",
		hash + ":":                          hash + ":",
		strings.ToUpper(hash) + ":a.go:F:1": strings.ToUpper(hash) + ":a.go:F:1",
	} {
		if got := graph.UnscopedID(in); got != want {
			t.Errorf("UnscopedID(%q) = %q, want %q", in, got, want)
		}
	}
}
