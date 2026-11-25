package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpToolTimeout bounds one tool call's index reads; cochange reads Git
// history and has mcpCochangeTimeout. They are variables so tests can
// shorten them.
var (
	mcpToolTimeout     = 2 * time.Second
	mcpCochangeTimeout = 10 * time.Second
)

// maxMCPBytes bounds the encoded structured result of every tool. Lists are
// shortened from the end, with truncated set, until the result fits.
const maxMCPBytes = 64 << 10

func mcpCmd() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Serve read-only code map tools over MCP (stdio)", Args: cobra.NoArgs, Long: `Serve the code map of --repo to an MCP client over stdin/stdout.

Tools (all read-only, bounded and repository-scoped): graph_search,
graph_def, graph_neighbors, graph_impact, graph_context and graph_cochange.
They read the last committed index generation and never create, migrate
or refresh it; run "trace-graph refresh" to update it.`, RunE: func(cmd *cobra.Command, _ []string) error {
		return newMCPServer(repoPath, dbPath).Run(cmd.Context(), &mcp.StdioTransport{})
	}}
}

type mcpServer struct{ repo, db string }

func newMCPServer(repo, db string) *mcp.Server {
	s := &mcpServer{repo: repo, db: db}
	server := mcp.NewServer(&mcp.Implementation{Name: "trace-graph", Version: Version}, nil)
	readOnly := func(title string) *mcp.ToolAnnotations {
		closed := false
		return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}
	}
	mcp.AddTool(server, &mcp.Tool{Name: "graph_search", Annotations: readOnly("Search symbols"), Description: "Find symbols (functions, methods, types, variables) in the indexed repository by name. " +
		"Words match name prefixes and split identifier words: \"analyze\" finds AnalyzeRepo and analyzeFile, \"http\" finds parse_http_request; literal signature words also match. " +
		"Results are ranked: exact name, then names starting with a word, then relevance. Optionally restrict to one repository-relative file. " +
		"Returns up to limit symbols (1-50, default 20) with file, line, kind, signature and an ID usable with graph_neighbors and graph_impact."}, s.search)
	mcp.AddTool(server, &mcp.Tool{Name: "graph_def", Annotations: readOnly("Symbol definitions"), Description: "List every definition of an exact symbol name with its file, line, kind and signature (overloads and same-named symbols in different files are all listed). " +
		"At most limit definitions (1-50, default 20)."}, s.def)
	mcp.AddTool(server, &mcp.Tool{Name: "graph_neighbors", Annotations: readOnly("Direct callers"), Description: "Show the definitions of a symbol (exact name or ID) and its direct callers: call sites resolved to it, interface dispatch sites, and unresolved calls of the same name. " +
		"Each list holds at most limit entries (1-50, default 20). Use graph_impact for transitive callers or callees."}, s.neighbors)
	mcp.AddTool(server, &mcp.Tool{Name: "graph_impact", Annotations: readOnly("Transitive impact"), Description: "Walk resolved call edges transitively from one symbol (exact name or ID; an ambiguous name is an error that asks for the ID). " +
		"direction is callers (default: what breaks if it changes), callees (what it depends on) or both; depth 1-20 (default 3); limit 1-200 symbols (default 50); include_dispatch also follows interface dispatch to possible implementations. " +
		"Unresolved and dynamic calls are never guessed. truncated or deadline_exceeded mark a partial, best-effort result."}, s.impact)
	mcp.AddTool(server, &mcp.Tool{Name: "graph_context", Annotations: readOnly("Code context"), Description: "Read bounded code context: ranked symbols matching query words (all symbols when query is empty), optionally within one repository-relative path, with up to 50 relationships (calls, imports, routes, implements) touching them. " +
		"semantic reranks candidates by local hashed-word embeddings of names, signatures and paths (not a learned model). limit is 1-50 symbols (default 20). " +
		"status is ready, missing_index or needs_refresh; indexed_at is the index time in Unix milliseconds."}, s.context)
	mcp.AddTool(server, &mcp.Tool{Name: "graph_cochange", Annotations: readOnly("Co-changing files"), Description: "List source file pairs that changed together in the same commits, most frequent first, from the last max_count commits of local Git history (1-1000, default 200). " +
		"Pairs changed together fewer than twice are omitted. Returns at most limit pairs (1-50, default 20). Requires a Git checkout; never fetches."}, s.cochange)
	return server
}

func mcpLimit(limit, fallback, maximum int) (int, error) {
	if limit == 0 {
		return fallback, nil
	}
	if limit < 1 || limit > maximum {
		return 0, fmt.Errorf("limit must be 1-%d", maximum)
	}
	return limit, nil
}

// fitMCP drops trailing list entries until out encodes within maxMCPBytes.
// shorten removes one entry and reports false when nothing is left to drop.
func fitMCP(out any, shorten func() bool) error {
	for {
		encoded, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(encoded) <= maxMCPBytes || !shorten() {
			return nil
		}
	}
}

type mcpSearchInput struct {
	Query string `json:"query" jsonschema:"symbol name words, for example: analyze repo"`
	Path  string `json:"path,omitempty" jsonschema:"optional repository-relative file to search within"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum symbols, 1-50 (default 20)"`
}

type mcpSymbolsOutput struct {
	Repo      string       `json:"repo"`
	Symbols   []codeSymbol `json:"symbols"`
	Truncated bool         `json:"truncated"`
}

func (s *mcpServer) search(ctx context.Context, _ *mcp.CallToolRequest, in mcpSearchInput) (*mcp.CallToolResult, mcpSymbolsOutput, error) {
	limit, err := mcpLimit(in.Limit, 20, 50)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	if len(in.Query) > 4096 || len(in.Path) > 4096 {
		return nil, mcpSymbolsOutput{}, fmt.Errorf("query or path exceeds 4096 bytes")
	}
	path, err := repositoryPath(in.Path)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	db, err := openReadIndex(s.db)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	defer db.Close()
	symbols, ok, err := searchSymbols(ctx, db, s.repo, in.Query, path, limit+1)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	if !ok {
		return nil, mcpSymbolsOutput{}, fmt.Errorf("query has no words to search for")
	}
	out := mcpSymbolsOutput{Repo: s.repo, Symbols: symbols}
	if len(out.Symbols) > limit {
		out.Symbols, out.Truncated = out.Symbols[:limit], true
	}
	return nil, out, fitMCP(&out, func() bool { return dropLast(&out.Symbols, &out.Truncated) })
}

type mcpNameInput struct {
	Name  string `json:"name" jsonschema:"exact symbol name"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum definitions, 1-50 (default 20)"`
}

func (s *mcpServer) def(ctx context.Context, _ *mcp.CallToolRequest, in mcpNameInput) (*mcp.CallToolResult, mcpSymbolsOutput, error) {
	limit, err := mcpLimit(in.Limit, 20, 50)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	if in.Name == "" || len(in.Name) > 4096 {
		return nil, mcpSymbolsOutput{}, fmt.Errorf("name must be 1-4096 bytes")
	}
	ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	db, err := openReadIndex(s.db)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	defer db.Close()
	defs, err := lookupDefinitions(ctx, db, s.repo, in.Name, limit+1)
	if err != nil {
		return nil, mcpSymbolsOutput{}, err
	}
	out := mcpSymbolsOutput{Repo: s.repo, Symbols: defs}
	if len(out.Symbols) > limit {
		out.Symbols, out.Truncated = out.Symbols[:limit], true
	}
	return nil, out, fitMCP(&out, func() bool { return dropLast(&out.Symbols, &out.Truncated) })
}

type mcpSymbolInput struct {
	Symbol string `json:"symbol" jsonschema:"exact symbol name or symbol ID"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum definitions and callers each, 1-50 (default 20)"`
}

type mcpNeighborsOutput struct {
	Repo string `json:"repo"`
	neighborsResult
	Truncated bool `json:"truncated"`
}

func (s *mcpServer) neighbors(ctx context.Context, _ *mcp.CallToolRequest, in mcpSymbolInput) (*mcp.CallToolResult, mcpNeighborsOutput, error) {
	limit, err := mcpLimit(in.Limit, 20, 50)
	if err != nil {
		return nil, mcpNeighborsOutput{}, err
	}
	if in.Symbol == "" || len(in.Symbol) > 4096 {
		return nil, mcpNeighborsOutput{}, fmt.Errorf("symbol must be 1-4096 bytes")
	}
	ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	db, err := openReadIndex(s.db)
	if err != nil {
		return nil, mcpNeighborsOutput{}, err
	}
	defer db.Close()
	result, err := lookupNeighbors(ctx, db, s.repo, in.Symbol, limit+1)
	if err != nil {
		return nil, mcpNeighborsOutput{}, err
	}
	out := mcpNeighborsOutput{Repo: s.repo, neighborsResult: result}
	if len(out.Definitions) > limit {
		out.Definitions, out.Truncated = out.Definitions[:limit], true
	}
	if len(out.Callers) > limit {
		out.Callers, out.Truncated = out.Callers[:limit], true
	}
	return nil, out, fitMCP(&out, func() bool {
		return dropLast(&out.Callers, &out.Truncated) || dropLast(&out.Definitions, &out.Truncated)
	})
}

type mcpImpactInput struct {
	Symbol          string `json:"symbol" jsonschema:"exact symbol name or symbol ID"`
	Direction       string `json:"direction,omitempty" jsonschema:"callers (default), callees or both"`
	Depth           int    `json:"depth,omitempty" jsonschema:"maximum graph distance, 1-20 (default 3)"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum returned symbols, 1-200 (default 50)"`
	IncludeDispatch bool   `json:"include_dispatch,omitempty" jsonschema:"also follow interface dispatch edges to possible implementations"`
}

func (s *mcpServer) impact(ctx context.Context, _ *mcp.CallToolRequest, in mcpImpactInput) (*mcp.CallToolResult, impactReport, error) {
	limit, err := mcpLimit(in.Limit, 50, 200)
	if err != nil {
		return nil, impactReport{}, err
	}
	if in.Direction == "" {
		in.Direction = "callers"
	}
	if in.Depth == 0 {
		in.Depth = 3
	}
	if err := checkImpactRequest(in.Symbol, in.Direction, in.Depth, limit); err != nil {
		return nil, impactReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	db, err := openReadIndex(s.db)
	if err != nil {
		return nil, impactReport{}, err
	}
	defer db.Close()
	out, err := impactFor(ctx, db, s.repo, in.Symbol, in.Direction, in.Depth, limit, in.IncludeDispatch)
	if err != nil {
		return nil, impactReport{}, err
	}
	return nil, out, fitMCP(&out, func() bool { return dropLast(&out.Nodes, &out.Truncated) })
}

type mcpContextInput struct {
	Query    string `json:"query,omitempty" jsonschema:"symbol words; empty lists symbols in file and line order"`
	Path     string `json:"path,omitempty" jsonschema:"optional repository-relative file"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum symbols, 1-50 (default 20)"`
	Semantic bool   `json:"semantic,omitempty" jsonschema:"rerank candidates by local embedding similarity"`
}

func (s *mcpServer) context(ctx context.Context, _ *mcp.CallToolRequest, in mcpContextInput) (*mcp.CallToolResult, codeContext, error) {
	limit, err := mcpLimit(in.Limit, 20, 50)
	if err != nil {
		return nil, codeContext{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()
	out, err := codeContextFor(ctx, s.db, s.repo, in.Path, in.Query, limit, in.Semantic)
	if err != nil {
		return nil, codeContext{}, err
	}
	return nil, out, fitMCP(&out, func() bool {
		return dropLast(&out.Relations, &out.Truncated) || dropLast(&out.Symbols, &out.Truncated)
	})
}

type mcpCochangeInput struct {
	MaxCount int `json:"max_count,omitempty" jsonschema:"commits to scan, 1-1000 (default 200)"`
	Limit    int `json:"limit,omitempty" jsonschema:"maximum pairs, 1-50 (default 20)"`
}

type mcpCochangeOutput struct {
	Repo      string         `json:"repo"`
	Commits   int            `json:"commits_scanned_max"`
	Pairs     []cochangePair `json:"pairs"`
	Truncated bool           `json:"truncated"`
}

func (s *mcpServer) cochange(ctx context.Context, _ *mcp.CallToolRequest, in mcpCochangeInput) (*mcp.CallToolResult, mcpCochangeOutput, error) {
	limit, err := mcpLimit(in.Limit, 20, 50)
	if err != nil {
		return nil, mcpCochangeOutput{}, err
	}
	if in.MaxCount == 0 {
		in.MaxCount = 200
	}
	ctx, cancel := context.WithTimeout(ctx, mcpCochangeTimeout)
	defer cancel()
	pairs, err := cochangePairs(ctx, s.repo, in.MaxCount, 100000, 8<<20)
	if err != nil {
		return nil, mcpCochangeOutput{}, err
	}
	out := mcpCochangeOutput{Repo: s.repo, Commits: in.MaxCount, Pairs: pairs}
	if len(out.Pairs) > limit {
		out.Pairs, out.Truncated = out.Pairs[:limit], true
	}
	return nil, out, fitMCP(&out, func() bool { return dropLast(&out.Pairs, &out.Truncated) })
}

// dropLast removes the last element of a non-empty list and marks the
// result truncated.
func dropLast[T any](list *[]T, truncated *bool) bool {
	if len(*list) == 0 {
		return false
	}
	*list = (*list)[:len(*list)-1]
	*truncated = true
	return true
}
