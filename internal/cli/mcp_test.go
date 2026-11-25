package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectMCP(t *testing.T, repo, db string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := newMCPServer(repo, db).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// callMCP calls a tool and decodes its structured result into out. It
// returns the tool error text, or "" on success.
func callMCP(t *testing.T, client *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		var text []string
		for _, content := range result.Content {
			if c, ok := content.(*mcp.TextContent); ok {
				text = append(text, c.Text)
			}
		}
		return strings.Join(text, " ")
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxMCPBytes {
		t.Fatalf("%s returned %d bytes", name, len(encoded))
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatalf("%s: %v: %s", name, err, encoded)
	}
	return ""
}

func TestMCPToolsAreReadOnlyAndAnswerFromTheIndex(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.db")
	compareTestGit(t, root, "init", "-q")
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		write("a.go", fmt.Sprintf("package p\n\n// revision %d\nfunc AnalyzeRepo() { analyzeFile() }\n", i))
		write("b.go", fmt.Sprintf("package p\n\n// revision %d\nfunc analyzeFile() {}\nfunc Caller() { AnalyzeRepo() }\n", i))
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
	}
	client := connectMCP(t, root, path)

	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Description == "" {
			t.Fatalf("%s is not described as read-only: %+v", tool.Name, tool.Annotations)
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "graph_cochange,graph_context,graph_def,graph_impact,graph_neighbors,graph_search" {
		t.Fatalf("tools: %s", got)
	}

	// Before an index exists, index tools fail without creating one.
	var symbols mcpSymbolsOutput
	if msg := callMCP(t, client, "graph_search", map[string]any{"query": "analyze"}, &symbols); !strings.Contains(msg, "run trace-graph refresh") {
		t.Fatalf("missing index: %q", msg)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a tool created the index: %v", err)
	}
	var missing codeContext
	if msg := callMCP(t, client, "graph_context", map[string]any{"query": "analyze"}, &missing); msg != "" || missing.Status != "missing_index" {
		t.Fatalf("context without index: %q %+v", msg, missing)
	}

	runContextCLI(t, root, path, "refresh")

	if msg := callMCP(t, client, "graph_search", map[string]any{"query": "analyze", "limit": 1}, &symbols); msg != "" {
		t.Fatal(msg)
	}
	if len(symbols.Symbols) != 1 || !symbols.Truncated || symbols.Repo != root {
		t.Fatalf("search limit: %+v", symbols)
	}
	if msg := callMCP(t, client, "graph_search", map[string]any{"query": "analyze"}, &symbols); msg != "" || len(symbols.Symbols) != 2 || symbols.Truncated {
		t.Fatalf("search: %q %+v", msg, symbols)
	}
	if msg := callMCP(t, client, "graph_search", map[string]any{"query": "analyze", "limit": 51}, &symbols); !strings.Contains(msg, "limit must be 1-50") {
		t.Fatalf("unbounded limit accepted: %q", msg)
	}

	if msg := callMCP(t, client, "graph_def", map[string]any{"name": "analyzeFile"}, &symbols); msg != "" || len(symbols.Symbols) != 1 || symbols.Symbols[0].File != "b.go" || symbols.Symbols[0].Line != 4 {
		t.Fatalf("def: %q %+v", msg, symbols)
	}

	// Tree-sitter builds observe calls but leave their targets unresolved.
	resolved := !strings.HasPrefix(graph.Capabilities().Analyzer, "treesitter")
	var neighbors mcpNeighborsOutput
	if msg := callMCP(t, client, "graph_neighbors", map[string]any{"symbol": "AnalyzeRepo"}, &neighbors); msg != "" || len(neighbors.Definitions) != 1 {
		t.Fatalf("neighbors: %q %+v", msg, neighbors)
	}
	if resolved && (len(neighbors.Callers) != 1 || neighbors.Callers[0].Symbol != "Caller") {
		t.Fatalf("neighbors callers: %+v", neighbors)
	}

	var impact impactReport
	if msg := callMCP(t, client, "graph_impact", map[string]any{"symbol": "analyzeFile"}, &impact); msg != "" {
		t.Fatal(msg)
	}
	var chain []string
	for _, node := range impact.Nodes {
		chain = append(chain, fmt.Sprintf("%s@%d", node.Name, node.Distance))
	}
	want := "AnalyzeRepo@1,Caller@2"
	if !resolved {
		want = ""
	}
	if impact.Direction != "callers" || impact.Depth != 3 || strings.Join(chain, ",") != want {
		t.Fatalf("impact: %+v %v", impact, chain)
	}
	if msg := callMCP(t, client, "graph_impact", map[string]any{"symbol": "analyzeFile", "direction": "sideways"}, &impact); !strings.Contains(msg, "direction must be") {
		t.Fatalf("invalid direction: %q", msg)
	}

	var code codeContext
	if msg := callMCP(t, client, "graph_context", map[string]any{"query": "caller", "limit": 5}, &code); msg != "" || code.Status != "ready" || len(code.Symbols) != 1 || code.Symbols[0].Name != "Caller" || len(code.Relations) == 0 {
		t.Fatalf("context: %q %+v", msg, code)
	}
	if msg := callMCP(t, client, "graph_context", map[string]any{"path": "../outside.go"}, &code); !strings.Contains(msg, "relative to the repository") {
		t.Fatalf("escaping path accepted: %q", msg)
	}

	var cochange mcpCochangeOutput
	if msg := callMCP(t, client, "graph_cochange", map[string]any{"max_count": 10}, &cochange); msg != "" || len(cochange.Pairs) != 1 || cochange.Pairs[0].A != "a.go" || cochange.Pairs[0].B != "b.go" || cochange.Pairs[0].Count != 2 {
		t.Fatalf("cochange: %q %+v", msg, cochange)
	}
}

// Every list result is shortened to fit maxMCPBytes, and marked truncated.
func TestMCPResultsAreBoundedInBytes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.db")
	var src strings.Builder
	src.WriteString("package p\n")
	// Long names make every build's symbols large (Tree-sitter signatures
	// hold only the node kind and name).
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "func Wide%02d%s() {}\n", i, strings.Repeat("x", 1500))
	}
	if err := os.WriteFile(filepath.Join(root, "wide.go"), []byte(src.String()), 0600); err != nil {
		t.Fatal(err)
	}
	runContextCLI(t, root, path, "refresh")
	client := connectMCP(t, root, path)
	var symbols mcpSymbolsOutput
	if msg := callMCP(t, client, "graph_search", map[string]any{"query": "wide", "limit": 50}, &symbols); msg != "" {
		t.Fatal(msg)
	}
	if !symbols.Truncated || len(symbols.Symbols) == 0 || len(symbols.Symbols) >= 40 {
		t.Fatalf("large search result not bounded: %d symbols, truncated %v", len(symbols.Symbols), symbols.Truncated)
	}
	var code codeContext
	if msg := callMCP(t, client, "graph_context", map[string]any{"query": "wide", "limit": 50}, &code); msg != "" || !code.Truncated {
		t.Fatalf("large context result not bounded: %q %d", msg, len(code.Symbols))
	}
}
