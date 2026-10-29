package cli

import (
	"bufio"
	"database/sql"
	"os/exec"
	"strings"
	"fmt"
	"os"
	"path/filepath"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/UseTrace-SI/trace-graph/internal/store"
	"github.com/spf13/cobra"
)

var dbPath string
var repoPath string

func Execute() error {
	root := &cobra.Command{Use: "trace-graph", Short: "Code map for Trace"}
	root.PersistentFlags().StringVar(&dbPath, "db", ".trace-graph/graph.db", "graph database path")
	root.PersistentFlags().StringVar(&repoPath, "repo", ".", "repository root")
	root.AddCommand(versionCmd())
	root.AddCommand(analyzeCmd())
	root.AddCommand(symbolsCmd())
	root.AddCommand(queryCmd())
	root.AddCommand(neighborsCmd())
	root.AddCommand(impactCmd())
	root.AddCommand(defCmd())
	root.AddCommand(cochangeCmd())
	root.AddCommand(capabilitiesCmd())
	return root.Execute()
}

func openDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	return store.Open(dbPath)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("trace-graph dev")
	}}
}

func analyzeCmd() *cobra.Command {
	return &cobra.Command{Use: "analyze", RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		n, err := graph.AnalyzeRepo(repoPath, db)
		if err != nil {
			return err
		}
		fmt.Printf("analyzed %d symbols\n", n)
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
		rows, err := db.Query("SELECT file,name,kind,line FROM symbols ORDER BY file,line")
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
			fmt.Printf("%s:%d  %s  %s\n", file, line, kind, name)
		}
		return rows.Err()
	}}
}

func queryCmd() *cobra.Command {
	return &cobra.Command{Use: "query", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		q := args[0]
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query("SELECT s.file,s.name,s.kind,s.line FROM symbols s JOIN symbol_fts f ON f.rowid=s.rowid WHERE symbol_fts MATCH ? ORDER BY s.file,s.line", q)
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
			fmt.Printf("%s:%d  %s  %s\n", file, line, kind, name)
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
		rows, err := db.Query("SELECT file,line,kind FROM symbols WHERE name=?", name)
		if err != nil {
			return err
		}
		fmt.Println("definitions:")
		for rows.Next() {
			var file, kind string
			var line int
			rows.Scan(&file, &kind, &line)
			fmt.Printf("  %s:%d  %s\n", file, line, kind)
		}
		rows.Close()
		r2, err := db.Query("SELECT DISTINCT from_symbol,file,line FROM relations WHERE to_symbol=? ORDER BY file,line", name)
		if err != nil {
			return err
		}
		defer r2.Close()
		fmt.Println("callers (neighbors):")
		for r2.Next() {
			var sym, file string
			var line int
			r2.Scan(&sym, &file, &line)
			fmt.Printf("  %s  %s:%d\n", sym, file, line)
		}
		return nil
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
		rows, err := db.Query("SELECT DISTINCT file FROM relations WHERE to_symbol=? OR from_symbol=?", name, name)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Printf("files potentially affected by %s:\n", name)
		for rows.Next() {
			var f string
			rows.Scan(&f)
			fmt.Printf("  %s\n", f)
		}
		return nil
	}}
}

func defCmd() *cobra.Command {
	return &cobra.Command{Use: "def", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.Query("SELECT file,line,kind,signature FROM symbols WHERE name=?", args[0])
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var file, kind, sig string
			var line int
			rows.Scan(&file, &line, &kind, &sig)
			fmt.Printf("%s:%d  [%s] %s\n", file, line, kind, sig)
		}
		return nil
	}}
}


func capabilitiesCmd() *cobra.Command {
	return &cobra.Command{Use: "capabilities", Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("semantic (treesitter build): go, python, javascript, typescript, rust, java, c, cpp, ruby")
		fmt.Println("inventory (default build): python, javascript, typescript, rust, java, c, cpp, ruby, and others")
		fmt.Println("relationships: calls (go, python, javascript, typescript, rust, java, c, cpp, ruby)")
		fmt.Println("co-change: git history based")
		fmt.Println("cache: mtime-based file_cache")
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
					if a > b { a, b = b, a }
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
			fmt.Printf("%d  %s  <->  %s\n", e.n, e.a, e.b)
		}
		if len(entries) == 0 {
			fmt.Println("no co-change data")
		}
		return nil
	}}
}
