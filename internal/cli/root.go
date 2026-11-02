package cli

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	root.AddCommand(impactCmd())
	root.AddCommand(defCmd())
	root.AddCommand(cochangeCmd())
	root.AddCommand(capabilitiesCmd())
	root.AddCommand(contextCmd())
	root.AddCommand(refreshCmd())
	return root
}

func Execute() error { return newRoot().Execute() }

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
		db, err := openDB()
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
		q, err := store.LiteralQuery(strings.Join(args, " "))
		if err != nil {
			return err
		}
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query("SELECT s.file,s.name,s.kind,s.line FROM symbols s JOIN symbol_fts f ON f.rowid=s.rowid WHERE symbol_fts MATCH ? AND s.repo=? ORDER BY s.file,s.line", q, repoPath)
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

func neighborsCmd() *cobra.Command {
	return &cobra.Command{Use: "neighbors", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		name := args[0]
		rows, err := db.Query("SELECT file,line,kind FROM symbols WHERE name=? AND repo=?", name, repoPath)
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
		r2, err := db.Query("SELECT DISTINCT from_symbol,file,line FROM relations WHERE to_symbol=? AND repo=? ORDER BY file,line", name, repoPath)
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

func impactCmd() *cobra.Command {
	return &cobra.Command{Use: "impact", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		name := args[0]
		rows, err := db.Query("SELECT DISTINCT file FROM relations WHERE repo=? AND (to_symbol=? OR from_symbol=?)", repoPath, name, name)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Fprintf(cmd.OutOrStdout(), "files potentially affected by %s:\n", name)
		for rows.Next() {
			var f string
			if err := rows.Scan(&f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", f)
		}
		return rows.Err()
	}}
}

func defCmd() *cobra.Command {
	return &cobra.Command{Use: "def", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
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
		fmt.Fprintln(cmd.OutOrStdout(), "semantic (treesitter build): go, python, javascript, typescript, rust, java, c, cpp, ruby")
		fmt.Fprintln(cmd.OutOrStdout(), "inventory (default build): python, javascript, typescript, rust, java, c, cpp, ruby, and others")
		fmt.Fprintln(cmd.OutOrStdout(), "relationships: calls (go, python, javascript, typescript, rust, java, c, cpp, ruby)")
		fmt.Fprintln(cmd.OutOrStdout(), "co-change: git history based")
		fmt.Fprintln(cmd.OutOrStdout(), "cache: repository-scoped content hashes")
	}}
}

func cochangeCmd() *cobra.Command {
	return &cobra.Command{Use: "cochange", RunE: func(cmd *cobra.Command, args []string) error {
		// Use git log to find files changed together
		gitcmd := exec.Command("git", "-C", repoPath, "log", "--name-only", "--pretty=format:")
		outb, err := gitcmd.Output()
		out := string(outb)
		if err != nil {
			return err
		}
		// count co-occurrences
		type pair struct{ a, b string }
		counts := map[pair]int{}
		scanner := bufio.NewScanner(strings.NewReader(out))
		var current []string
		flush := func() {
			for i := 0; i < len(current); i++ {
				for j := i + 1; j < len(current); j++ {
					a, b := current[i], current[j]
					if a > b {
						a, b = b, a
					}
					counts[pair{a, b}]++
				}
			}
			current = nil
		}
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				flush()
				continue
			}
			current = append(current, line)
		}
		flush()
		// print top
		type entry struct {
			a, b string
			n    int
		}
		var entries []entry
		for p, n := range counts {
			if n >= 2 {
				entries = append(entries, entry{p.a, p.b, n})
			}
		}
		// sort by n desc
		// simple bubble
		for i := 0; i < len(entries); i++ {
			for j := i + 1; j < len(entries); j++ {
				if entries[j].n > entries[i].n {
					entries[i], entries[j] = entries[j], entries[i]
				}
			}
		}
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
}
