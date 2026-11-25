package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/spf13/cobra"
)

// Inspection must never create or migrate an index. A missing index is an
// explicit error with an actionable refresh command.
func readIndex() (*sql.DB, error) { return openReadIndex(dbPath) }

func openReadIndex(file string) (*sql.DB, error) {
	path, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("read index (run trace-graph refresh first): %w", err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

type impactNode struct {
	codeSymbol
	Distance int    `json:"distance"`
	Via      string `json:"via"`
}
type impactReport struct {
	Protocol         int            `json:"protocol"`
	Repo             string         `json:"repo"`
	IndexedAt        int64          `json:"indexed_at"`
	Symbol           codeSymbol     `json:"symbol"`
	Direction        string         `json:"direction"`
	Depth            int            `json:"depth"`
	Nodes            []impactNode   `json:"nodes"`
	IncludeDispatch  bool           `json:"include_dispatch"`
	Truncated        bool           `json:"truncated"`
	DeadlineExceeded bool           `json:"deadline_exceeded"`
	Analysis         graph.Analysis `json:"analysis"`
}

func transitiveImpactCmd() *cobra.Command {
	var direction string
	var depth, limit int
	var jsonOutput, includeDispatch bool
	cmd := &cobra.Command{Use: "impact <symbol-or-id>", Short: "Trace resolved callers or callees transitively", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkImpactRequest(args[0], direction, depth, limit); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		out, err := impactFor(ctx, db, repoPath, args[0], direction, depth, limit, includeDispatch)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s of %s (%s:%d), indexed %d:\n", direction, out.Symbol.Name, out.Symbol.File, out.Symbol.Line, out.IndexedAt)
		for _, n := range out.Nodes {
			fmt.Fprintf(cmd.OutOrStdout(), "  depth %d  %s:%d  %s [%s]\n", n.Distance, n.File, n.Line, n.Name, n.Kind)
		}
		if out.DeadlineExceeded {
			fmt.Fprintln(cmd.OutOrStdout(), "Partial result: two-second deadline reached; best-effort result.")
		} else if out.Truncated {
			fmt.Fprintln(cmd.OutOrStdout(), "Partial result: depth or result limit reached.")
		}
		return nil
	}}
	cmd.Flags().StringVar(&direction, "direction", "callers", "callers, callees or both")
	cmd.Flags().IntVar(&depth, "depth", 3, "maximum graph distance (1–20)")
	cmd.Flags().IntVar(&limit, "limit", 200, "maximum returned symbols (1–1000)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit a protocol-1 JSON report")
	cmd.Flags().BoolVar(&includeDispatch, "include-dispatch", false, "also follow interface dispatch edges to possible implementations")
	return cmd
}

func checkImpactRequest(selector, direction string, depth, limit int) error {
	if direction != "callers" && direction != "callees" && direction != "both" {
		return fmt.Errorf("direction must be callers, callees or both")
	}
	if depth < 1 || depth > 20 || limit < 1 || limit > 1000 {
		return fmt.Errorf("depth must be 1–20 and limit 1–1000")
	}
	if len(selector) > 4096 {
		return fmt.Errorf("symbol selector exceeds 4096 bytes")
	}
	return nil
}

// impactFor validates an impact request and walks resolved edges from the
// single symbol named or identified by selector.
func impactFor(ctx context.Context, db *sql.DB, repo, selector, direction string, depth, limit int, includeDispatch bool) (impactReport, error) {
	if err := checkImpactRequest(selector, direction, depth, limit); err != nil {
		return impactReport{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return impactReport{}, err
	}
	defer tx.Rollback()
	out := impactReport{Protocol: 1, Repo: repo, Direction: direction, Depth: depth, IncludeDispatch: includeDispatch, Nodes: []impactNode{}, Analysis: graph.Capabilities()}
	if err := tx.QueryRowContext(ctx, "SELECT indexed_at FROM repo_state WHERE repo=?", repo).Scan(&out.IndexedAt); err != nil {
		return impactReport{}, fmt.Errorf("index unavailable; run trace-graph refresh: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,file,name,kind,line,substr(COALESCE(signature,''),1,2048) FROM symbols WHERE repo=? AND (name=? OR id IN (?,?)) ORDER BY file,line,id LIMIT 2", repo, selector, selector, graph.ScopedID(repo, selector))
	if err != nil {
		return impactReport{}, err
	}
	var roots []codeSymbol
	for rows.Next() {
		var s codeSymbol
		if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature); err != nil {
			rows.Close()
			return impactReport{}, err
		}
		roots = append(roots, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return impactReport{}, err
	}
	if len(roots) == 0 {
		return impactReport{}, fmt.Errorf("symbol not found")
	}
	if len(roots) > 1 {
		return impactReport{}, fmt.Errorf("ambiguous symbol; use the symbol ID from context")
	}
	out.Symbol = roots[0]
	if err := expandImpact(ctx, tx, &out, repo, direction, depth, limit); err != nil {
		return impactReport{}, err
	}
	// The walk follows stored IDs; the report shows them unscoped.
	out.Symbol.ID = graph.UnscopedID(out.Symbol.ID)
	for i := range out.Nodes {
		out.Nodes[i].ID, out.Nodes[i].Via = graph.UnscopedID(out.Nodes[i].ID), graph.UnscopedID(out.Nodes[i].Via)
	}
	return out, nil
}

// expandImpact walks resolved edges breadth-first from out.Symbol. When ctx
// expires mid-walk it stops and marks the report best-effort instead of
// failing, so a deep graph cannot break the command's responsiveness budget.
func expandImpact(ctx context.Context, tx *sql.Tx, out *impactReport, repo, direction string, depth, limit int) error {
	deadline := func() {
		out.Truncated = true
		out.DeadlineExceeded = true
	}
	queue := []impactNode{{codeSymbol: out.Symbol}}
	visited := map[string]bool{out.Symbol.ID: true}
	for cursor := 0; cursor < len(queue); cursor++ {
		if ctx.Err() != nil {
			deadline()
			return nil
		}
		current := queue[cursor]
		join, filter := "s.id=t.from_id", "t.to_id=?"
		params := []any{repo, repo, current.ID}
		if direction == "callees" {
			join, filter = "s.id=t.to_id", "t.from_id=?"
		}
		if direction == "both" {
			join, filter = "s.id=CASE WHEN t.from_id=? THEN t.to_id ELSE t.from_id END", "(t.to_id=? OR t.from_id=?)"
			params = []any{current.ID, repo, repo, current.ID, current.ID}
		}
		params = append(params, limit+1)
		kinds := "r.kind='call'"
		if out.IncludeDispatch {
			kinds = "r.kind IN ('call','dispatch')"
		}
		q := `SELECT DISTINCT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048) FROM relation_targets t JOIN relations r ON r.id=t.relation_id JOIN symbols s ON ` + join + ` WHERE r.repo=? AND s.repo=? AND t.to_id IS NOT NULL AND ` + kinds + ` AND ` + filter + ` ORDER BY s.file,s.line,s.id LIMIT ?`
		rows, err := tx.QueryContext(ctx, q, params...)
		if err != nil {
			if ctx.Err() != nil {
				deadline()
				return nil
			}
			return err
		}
		count := 0
		for rows.Next() {
			if ctx.Err() != nil {
				rows.Close()
				deadline()
				return nil
			}
			count++
			var s codeSymbol
			if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature); err != nil {
				rows.Close()
				return err
			}
			if visited[s.ID] {
				continue
			}
			if current.Distance >= depth || len(out.Nodes) >= limit {
				out.Truncated = true
				continue
			}
			visited[s.ID] = true
			n := impactNode{codeSymbol: s, Distance: current.Distance + 1, Via: current.ID}
			out.Nodes = append(out.Nodes, n)
			queue = append(queue, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			if ctx.Err() != nil {
				deadline()
				return nil
			}
			return err
		}
		if count > limit {
			out.Truncated = true
		}
	}
	return nil
}

func countGroups(ctx context.Context, db *sql.DB, query string, args ...any) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		counts[key] = n
	}
	return counts, rows.Err()
}

func healthCmd() *cobra.Command {
	return &cobra.Command{Use: "health", Short: "Report index age, coverage and unresolved relationships as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		defer cancel()
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		var indexed int64
		var files, symbols, calls, resolved int
		err = db.QueryRowContext(ctx, `SELECT indexed_at,(SELECT count(*) FROM file_cache_v2 WHERE repo=?),(SELECT count(*) FROM symbols WHERE repo=?),(SELECT count(*) FROM relations WHERE repo=? AND kind='call'),(SELECT count(*) FROM relation_targets t JOIN relations r ON r.id=t.relation_id JOIN symbols s ON s.id=t.to_id AND s.repo=r.repo WHERE r.repo=? AND r.kind='call') FROM repo_state WHERE repo=?`, repoPath, repoPath, repoPath, repoPath, repoPath).Scan(&indexed, &files, &symbols, &calls, &resolved)
		if err != nil {
			return fmt.Errorf("index unavailable; run trace-graph refresh: %w", err)
		}
		byResolution, err := countGroups(ctx, db, `SELECT t.resolution,count(*) FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.repo=? AND r.kind='call' GROUP BY t.resolution`, repoPath)
		if err != nil {
			return err
		}
		byKind, err := countGroups(ctx, db, `SELECT kind,count(*) FROM relations WHERE repo=? GROUP BY kind`, repoPath)
		if err != nil {
			return err
		}
		report := map[string]any{"protocol": 1, "repo": repoPath, "indexed_at": indexed, "files": files, "symbols": symbols, "calls": calls, "resolved_calls": resolved, "unresolved_calls": calls - resolved, "call_resolution": byResolution, "relations": byKind, "analysis": graph.Capabilities()}
		// Sources left out of the index, such as files whose declarations
		// cannot be given distinct identities.
		skipped, err := graph.FileWarnings(ctx, db, repoPath, 20)
		if err != nil {
			return err
		}
		var skippedCount int
		if len(skipped) > 0 {
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM file_warnings WHERE repo=?", repoPath).Scan(&skippedCount); err != nil {
				return err
			}
		}
		report["skipped_files"] = skippedCount
		report["skipped"] = skipped
		// Indexes written before package-wide type checking have no pass record.
		var detail string
		if err := db.QueryRowContext(ctx, "SELECT detail FROM repo_resolution WHERE repo=?", repoPath).Scan(&detail); err == nil {
			report["go_types"] = json.RawMessage(detail)
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}}
}
