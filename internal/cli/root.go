package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/UseTrace-SI/trace-graph/internal/store"
	"github.com/spf13/cobra"
)

var dbPath string
var repoPath string

func newRoot() *cobra.Command {
	root := &cobra.Command{Use: "trace-graph", Short: "Code map for Trace", Long: `Code map for Trace: indexes a repository's files, symbols and relationships
(calls, imports, routes, implements) into a local SQLite database and answers
lookups from it.

Quick start:
  trace-graph analyze                    index the current checkout (incremental)
  trace-graph query analyze repo         ranked symbol search by name and words
  trace-graph def AnalyzeRepo            definitions and signatures
  trace-graph neighbors AnalyzeRepo      direct callers
  trace-graph impact AnalyzeRepo         transitive callers (--direction callees)
  trace-graph context --query "parse config" --semantic
  trace-graph mcp                        serve the same lookups to an MCP client`}
	root.PersistentFlags().StringVar(&dbPath, "db", "", "graph database path (default <repo>/.trace-graph/graph.db, or <repo>/.trace/graph/graph.db in a Trace project)")
	root.PersistentFlags().StringVar(&repoPath, "repo", ".", "repository root")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Name() == "version" || cmd.Name() == "capabilities" {
			return nil
		}
		var err error
		repoPath, err = filepath.Abs(repoPath)
		if err != nil {
			return err
		}
		repoPath, err = filepath.EvalSymlinks(repoPath)
		if err != nil {
			return err
		}
		if !root.PersistentFlags().Changed("db") || dbPath == "" {
			dbPath = filepath.Join(repoPath, ".trace-graph", "graph.db")
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				if _, err := os.Stat(filepath.Join(repoPath, ".trace", "trace.db")); err == nil {
					dbPath = filepath.Join(repoPath, ".trace", "graph", "graph.db")
				}
			}
		}
		return nil
	}
	root.AddCommand(versionCmd())
	root.AddCommand(analyzeCmd())
	root.AddCommand(symbolsCmd())
	root.AddCommand(queryCmd())
	root.AddCommand(neighborsCmd())
	root.AddCommand(transitiveImpactCmd())
	root.AddCommand(healthCmd())
	root.AddCommand(snapshotCmd())
	root.AddCommand(changesCmd())
	root.AddCommand(compareCmd())
	root.AddCommand(defCmd())
	root.AddCommand(cochangeCmd())
	root.AddCommand(capabilitiesCmd())
	root.AddCommand(contextCmd())
	root.AddCommand(refreshCmd())
	root.AddCommand(mcpCmd())
	return root
}

func Execute() error { return newRoot().Execute() }

// openDB creates or migrates the index and therefore takes the SQLite writer.
// Only commands that write a generation use it; lookups use readIndex so they
// neither create an index nor wait behind an indexing transaction.
func openDB() (*sql.DB, error) {
	// The index holds source-derived symbols and signatures: keep a new
	// database directory private to its owner.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, err
	}
	return store.Open(dbPath)
}

// Version is set by the coordinated Trace bundle build.
var Version = "dev"

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the coordinated bundle version", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "trace-graph "+Version)
	}}
}

func analyzeCmd() *cobra.Command {
	return &cobra.Command{Use: "analyze", Short: "Index the repository (only changed files are re-analyzed)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		report, err := graph.AnalyzeRepoReport(cmd.Context(), repoPath, db)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "analyzed %d symbols\n", report.Symbols)
		for _, warning := range report.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %s\n", warning.Path, warning.Warning)
		}
		return nil
	}}
}

// symbolList is the --json form of symbols and query.
type symbolList struct {
	Protocol  int          `json:"protocol"`
	Repo      string       `json:"repo"`
	Symbols   []codeSymbol `json:"symbols"`
	Truncated bool         `json:"truncated"`
}

// printSymbols writes at most limit symbols (all when limit is 0) as lines
// or, with asJSON, as one symbolList. symbols may hold one extra entry
// that only marks the list truncated.
func printSymbols(cmd *cobra.Command, symbols []codeSymbol, limit int, asJSON bool) error {
	out := symbolList{Protocol: 1, Repo: repoPath, Symbols: publicSymbols(symbols)}
	if limit > 0 && len(out.Symbols) > limit {
		out.Symbols, out.Truncated = out.Symbols[:limit], true
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	for _, s := range out.Symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "%s:%d  %s  %s\n", s.File, s.Line, s.Kind, s.Name)
	}
	return nil
}

// probeLimit fetches one row beyond a positive limit, so printSymbols can
// tell a complete list from a truncated one. 0 stays unbounded.
func probeLimit(limit int) int {
	if limit > 0 {
		return limit + 1
	}
	return 0
}

func symbolsCmd() *cobra.Command {
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "symbols", Short: "List indexed symbols as file:line kind name", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if limit < 0 {
			return fmt.Errorf("limit must not be negative")
		}
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.QueryContext(cmd.Context(), "SELECT id,file,name,kind,line,substr(COALESCE(signature,''),1,2048) FROM symbols WHERE repo=? ORDER BY file,line,id LIMIT ?", repoPath, sqlLimit(probeLimit(limit)))
		if err != nil {
			return err
		}
		defer rows.Close()
		symbols := []codeSymbol{}
		for rows.Next() {
			var s codeSymbol
			if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature); err != nil {
				return err
			}
			symbols = append(symbols, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return printSymbols(cmd, symbols, limit, asJSON)
	}}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum symbols (0 lists all)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit protocol-1 JSON with IDs and signatures")
	return cmd
}

func queryCmd() *cobra.Command {
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "query <words>", Short: "Search symbols by name prefix and identifier words, best match first", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if limit < 0 {
			return fmt.Errorf("limit must not be negative")
		}
		text := strings.Join(args, " ")
		if len(text) > 4096 {
			return fmt.Errorf("query exceeds 4096 bytes")
		}
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		symbols, _, err := searchSymbols(cmd.Context(), db, repoPath, text, "", probeLimit(limit))
		if err != nil {
			return err
		}
		if symbols == nil {
			symbols = []codeSymbol{}
		}
		return printSymbols(cmd, symbols, limit, asJSON)
	}}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum symbols (0 lists every match)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit protocol-1 JSON with IDs and signatures")
	return cmd
}

func neighborsCmd() *cobra.Command {
	return &cobra.Command{Use: "neighbors <symbol-or-id>", Short: "Show a symbol's definitions and direct callers", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		result, err := lookupNeighbors(cmd.Context(), db, repoPath, args[0], 0)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "definitions:")
		for _, d := range result.Definitions {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s:%d  %s\n", d.File, d.Line, d.Kind)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "callers (neighbors):")
		for _, c := range result.Callers {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s:%d\n", c.Symbol, c.File, c.Line)
		}
		return nil
	}}
}

func defCmd() *cobra.Command {
	return &cobra.Command{Use: "def <name>", Short: "Show definitions and signatures for a symbol name", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		defs, err := lookupDefinitions(cmd.Context(), db, repoPath, args[0], 0)
		if err != nil {
			return err
		}
		for _, d := range defs {
			fmt.Fprintf(cmd.OutOrStdout(), "%s:%d  [%s] %s\n", d.File, d.Line, d.Kind, d.Signature)
		}
		return nil
	}}
}

func capabilitiesCmd() *cobra.Command {
	return &cobra.Command{Use: "capabilities", Short: "Describe the compiled analyzer and its limitations as JSON", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, args []string) {
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(graph.Capabilities())
	}}
}

// maxFilesForPairBudget returns the largest number of files whose pairwise
// combinations fit inside the unique-pair budget, never exceeding the scan
// safety cap. Without this, a single large commit (for example an initial
// import) can exhaust the default --max-pairs budget and fail cochange even
// though the user never exceeded the documented pair limit.
func maxFilesForPairBudget(maxPairs int) int {
	const scanCap = 500
	lo, hi := 0, scanCap
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if int64(mid)*(int64(mid)-1)/2 <= int64(maxPairs) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// cochangePair is a pair of source files changed together Count times.
type cochangePair struct {
	Count int    `json:"count"`
	A     string `json:"a"`
	B     string `json:"b"`
}

// cochangePairs reads a bounded, NUL-delimited git log of the last
// maxCount commits and returns the source file pairs changed together at
// least twice, most frequent first.
func cochangePairs(ctx context.Context, repo string, maxCount, maxPairs, maxBytes int) ([]cochangePair, error) {
	if maxCount < 1 || maxCount > 1000 || maxPairs < 1 || maxPairs > 100000 || maxBytes < 1024 || maxBytes > 64<<20 {
		return nil, fmt.Errorf("max-count must be 1-1000, max-pairs 1-100000 and max-log-bytes 1024-67108864")
	}
	// Stream a bounded log instead of loading all history into memory. The
	// isolated invocation cannot be redirected to another repository by
	// ambient GIT_* variables and never fetches missing objects.
	gitcmd := graph.LocalGitCommand(ctx, repo, "log", "-z", "--name-only", "--pretty=format:", "--max-count="+strconv.Itoa(maxCount))
	pipe, err := gitcmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	gitcmd.Stderr = &stderr
	if err := gitcmd.Start(); err != nil {
		return nil, err
	}
	waited := false
	defer func() {
		if !waited {
			_ = gitcmd.Process.Kill()
			_ = gitcmd.Wait()
		}
	}()
	maxFilesPerCommit := maxFilesForPairBudget(maxPairs)
	type pair struct{ a, b string }
	counts := map[pair]int{}
	limited := &io.LimitedReader{R: pipe, N: int64(maxBytes) + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	// Without -z Git quotes paths containing non-ASCII or special bytes
	// ("\303\274ber.go"), which then failed the source filter and were
	// silently dropped. NUL-delimited names are raw; an empty record still
	// separates commits.
	scanner.Split(splitNUL)
	var current []string
	overBudget := false
	flush := func() {
		for i := 0; i < len(current) && !overBudget; i++ {
			for j := i + 1; j < len(current); j++ {
				a, b := current[i], current[j]
				if a > b {
					a, b = b, a
				}
				// The budget bounds unique pairs: counting another
				// co-change of a known pair never exceeds it.
				key := pair{a, b}
				if _, known := counts[key]; !known && len(counts) >= maxPairs {
					overBudget = true
					break
				}
				counts[key]++
			}
		}
		current = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if !graph.CochangeFile(line) || len(current) >= maxFilesPerCommit {
			continue
		}
		current = append(current, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading git log: %w", err)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("cochange log exceeds %d bytes", maxBytes)
	}
	flush()
	waitErr := gitcmd.Wait()
	waited = true
	if waitErr != nil {
		return nil, fmt.Errorf("listing co-change history: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if overBudget {
		return nil, fmt.Errorf("cochange exceeds %d unique file pairs", maxPairs)
	}

	entries := make([]cochangePair, 0, len(counts))
	for p, n := range counts {
		if n >= 2 {
			entries = append(entries, cochangePair{Count: n, A: p.a, B: p.b})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		if entries[i].A != entries[j].A {
			return entries[i].A < entries[j].A
		}
		return entries[i].B < entries[j].B
	})
	return entries, nil
}

func cochangeCmd() *cobra.Command {
	var maxCount, maxPairs, maxBytes int
	cmd := &cobra.Command{Use: "cochange", Short: "Report files that change together", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		entries, err := cochangePairs(ctx, repoPath, maxCount, maxPairs, maxBytes)
		if err != nil {
			return err
		}
		for i, e := range entries {
			if i >= 20 {
				break
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  <->  %s\n", e.Count, e.A, e.B)
		}
		if len(entries) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no co-change data")
		}
		return nil
	}}
	cmd.Flags().IntVar(&maxCount, "max-count", 200, "maximum commits to scan (1-1000)")
	cmd.Flags().IntVar(&maxPairs, "max-pairs", 100000, "maximum unique file pairs (1-100000)")
	cmd.Flags().IntVar(&maxBytes, "max-log-bytes", 8<<20, "maximum git log bytes (1024-67108864)")
	return cmd
}
