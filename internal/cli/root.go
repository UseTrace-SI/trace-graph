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
	root := &cobra.Command{Use: "trace-graph", Short: "Code map for Trace"}
	root.PersistentFlags().StringVar(&dbPath, "db", ".trace-graph/graph.db", "graph database path")
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
		if !root.PersistentFlags().Changed("db") {
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
	return root
}

func Execute() error { return newRoot().Execute() }

// openDB creates or migrates the index and therefore takes the SQLite writer.
// Only commands that write a generation use it; lookups use readIndex so they
// neither create an index nor wait behind an indexing transaction.
func openDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	return store.Open(dbPath)
}

// Version is set by the coordinated Trace bundle build.
var Version = "dev"

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "trace-graph "+Version)
	}}
}

func analyzeCmd() *cobra.Command {
	return &cobra.Command{Use: "analyze", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		n, err := graph.AnalyzeRepoContext(cmd.Context(), repoPath, db)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "analyzed %d symbols\n", n)
		return nil
	}}
}

func symbolsCmd() *cobra.Command {
	return &cobra.Command{Use: "symbols", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query("SELECT file,name,kind,line FROM symbols WHERE repo=? ORDER BY file,line", repoPath)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var file, name, kind string
			var line int
			if err := rows.Scan(&file, &name, &kind, &line); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s:%d  %s  %s\n", file, line, kind, name)
		}
		return rows.Err()
	}}
}

func queryCmd() *cobra.Command {
	return &cobra.Command{Use: "query", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		text := strings.Join(args, " ")
		if len(text) > 4096 {
			return fmt.Errorf("query exceeds 4096 bytes")
		}
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		symbols, _, err := searchSymbols(cmd.Context(), db, repoPath, text, "", 0)
		if err != nil {
			return err
		}
		for _, s := range symbols {
			fmt.Fprintf(cmd.OutOrStdout(), "%s:%d  %s  %s\n", s.File, s.Line, s.Kind, s.Name)
		}
		return nil
	}}
}

func neighborsCmd() *cobra.Command {
	return &cobra.Command{Use: "neighbors", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		name := args[0]
		rows, err := db.Query("SELECT file,line,kind FROM symbols WHERE (name=? OR id=?) AND repo=?", name, name, repoPath)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "definitions:")
		for rows.Next() {
			var file, kind string
			var line int
			if err := rows.Scan(&file, &line, &kind); err != nil {
				rows.Close()
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s:%d  %s\n", file, line, kind)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		r2, err := db.Query(`SELECT DISTINCT r.from_symbol,r.file,r.line FROM relations r LEFT JOIN relation_targets t ON t.relation_id=r.id
 WHERE r.repo=? AND r.kind IN ('call','dispatch') AND (t.to_id IN (SELECT id FROM symbols WHERE repo=? AND (name=? OR id=?)) OR (t.relation_id IS NULL AND r.to_symbol=?)) ORDER BY r.file,r.line`, repoPath, repoPath, name, name, name)
		if err != nil {
			return err
		}
		defer r2.Close()
		fmt.Fprintln(cmd.OutOrStdout(), "callers (neighbors):")
		for r2.Next() {
			var sym, file string
			var line int
			if err := r2.Scan(&sym, &file, &line); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s:%d\n", sym, file, line)
		}
		return r2.Err()
	}}
}

func defCmd() *cobra.Command {
	return &cobra.Command{Use: "def", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query("SELECT file,line,kind,COALESCE(signature,'') FROM symbols WHERE name=? AND repo=?", args[0], repoPath)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var file, kind, sig string
			var line int
			if err := rows.Scan(&file, &line, &kind, &sig); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s:%d  [%s] %s\n", file, line, kind, sig)
		}
		return rows.Err()
	}}
}

func capabilitiesCmd() *cobra.Command {
	return &cobra.Command{Use: "capabilities", Run: func(cmd *cobra.Command, args []string) {
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

func cochangeCmd() *cobra.Command {
	var maxCount, maxPairs, maxBytes int
	cmd := &cobra.Command{Use: "cochange", Short: "Report files that change together", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if maxCount < 1 || maxCount > 1000 || maxPairs < 1 || maxPairs > 100000 || maxBytes < 1024 || maxBytes > 64<<20 {
			return fmt.Errorf("max-count must be 1-1000, max-pairs 1-100000 and max-log-bytes 1024-67108864")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		// Stream a bounded log instead of loading all history into memory. The
		// isolated invocation cannot be redirected to another repository by
		// ambient GIT_* variables and never fetches missing objects.
		gitcmd := graph.LocalGitCommand(ctx, repoPath, "log", "-z", "--name-only", "--pretty=format:", "--max-count="+strconv.Itoa(maxCount))
		pipe, err := gitcmd.StdoutPipe()
		if err != nil {
			return err
		}
		var stderr bytes.Buffer
		gitcmd.Stderr = &stderr
		if err := gitcmd.Start(); err != nil {
			return err
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
			if !graph.InspectableSource(line) || len(current) >= maxFilesPerCommit {
				continue
			}
			current = append(current, line)
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading git log: %w", err)
		}
		if limited.N <= 0 {
			return fmt.Errorf("cochange log exceeds %d bytes", maxBytes)
		}
		flush()
		waitErr := gitcmd.Wait()
		waited = true
		if waitErr != nil {
			return fmt.Errorf("listing co-change history: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		if overBudget {
			return fmt.Errorf("cochange exceeds %d unique file pairs", maxPairs)
		}

		type entry struct {
			a, b string
			n    int
		}
		entries := make([]entry, 0, len(counts))
		for p, n := range counts {
			if n >= 2 {
				entries = append(entries, entry{p.a, p.b, n})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].n != entries[j].n {
				return entries[i].n > entries[j].n
			}
			if entries[i].a != entries[j].a {
				return entries[i].a < entries[j].a
			}
			return entries[i].b < entries[j].b
		})
		for i, e := range entries {
			if i >= 20 {
				break
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  <->  %s\n", e.n, e.a, e.b)
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
