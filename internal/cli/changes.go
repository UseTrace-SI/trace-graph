package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/spf13/cobra"
)

type symbolChange struct {
	Kind   string           `json:"kind"`
	File   string           `json:"file"`
	Name   string           `json:"name"`
	Before []snapshotSymbol `json:"before"`
	After  []snapshotSymbol `json:"after"`
}

func changesCmd() *cobra.Command {
	var base string
	cmd := &cobra.Command{Use: "changes", Short: "Compare a complete graph snapshot with the current index", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if base == "" {
			return fmt.Errorf("--base requires a complete trace-graph snapshot file")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		previous, analyzer, err := readSnapshot(ctx, base, repoPath)
		if err != nil {
			return err
		}
		if analyzer != graph.Capabilities().Analyzer {
			return fmt.Errorf("snapshot analyzer differs; compare snapshots produced by the same parser build")
		}
		db, err := readIndex()
		if err != nil {
			return err
		}
		defer db.Close()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var indexed int64
		if err := tx.QueryRowContext(ctx, "SELECT indexed_at FROM repo_state WHERE repo=?", repoPath).Scan(&indexed); err != nil {
			return fmt.Errorf("index unavailable; run trace-graph refresh: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048),COALESCE(c.source_hash,''),COALESCE(c.end_line,s.line) FROM symbols s LEFT JOIN symbol_content c ON c.symbol_id=s.id WHERE s.repo=? ORDER BY s.file,s.line,s.id LIMIT 100001`, repoPath)
		if err != nil {
			return err
		}
		current := []snapshotSymbol{}
		for rows.Next() {
			var s snapshotSymbol
			if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature, &s.SourceHash, &s.EndLine); err != nil {
				rows.Close()
				return err
			}
			current = append(current, s)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(current) > 100000 {
			return fmt.Errorf("comparison exceeds 100000 symbols")
		}
		changes := compareSymbols(previous, current)
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "repo": repoPath, "indexed_at": indexed, "analysis": graph.Capabilities(), "changes": changes})
	}}
	cmd.Flags().StringVar(&base, "base", "", "complete NDJSON snapshot to compare against")
	return cmd
}

func compareSymbols(previous, current []snapshotSymbol) []symbolChange {
	before, after := groupSymbols(previous), groupSymbols(current)
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	changes := []symbolChange{}
	for _, key := range ordered {
		a, b := before[key], after[key]
		if len(a) > 1 && sameSnapshotSymbols(a, b) {
			continue
		}
		kind := ""
		switch {
		case len(a) > 1 || len(b) > 1:
			kind = "ambiguous"
		case len(a) == 0:
			kind = "added"
		case len(b) == 0:
			kind = "removed"
		case a[0].Signature != b[0].Signature:
			kind = "signature_changed"
		case a[0].SourceHash == "" || b[0].SourceHash == "":
			kind = "unverified"
		case a[0].SourceHash != b[0].SourceHash:
			kind = "source_changed"
		case a[0].Line != b[0].Line:
			kind = "moved"
		}
		if kind == "" {
			continue
		}
		sample := a
		if len(sample) == 0 {
			sample = b
		}
		if a == nil {
			a = []snapshotSymbol{}
		}
		if b == nil {
			b = []snapshotSymbol{}
		}
		changes = append(changes, symbolChange{Kind: kind, File: sample[0].File, Name: sample[0].Name, Before: a, After: b})
	}
	return changes
}

func sameSnapshotSymbols(a, b []snapshotSymbol) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func groupSymbols(symbols []snapshotSymbol) map[string][]snapshotSymbol {
	result := map[string][]snapshotSymbol{}
	for _, s := range symbols {
		key := s.File + "\x00" + s.Kind + "\x00" + s.Name
		result[key] = append(result[key], s)
	}
	return result
}

func readSnapshot(ctx context.Context, path, repo string) ([]snapshotSymbol, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return nil, "", fmt.Errorf("snapshot must be a regular file of at most 64 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	scan := bufio.NewScanner(io.LimitReader(f, (64<<20)+1))
	scan.Buffer(make([]byte, 4096), 65536)
	metadata, complete := false, false
	records := 0
	symbols := []snapshotSymbol{}
	analyzer := ""
	seen := map[string]bool{}
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if complete {
			return nil, "", fmt.Errorf("snapshot contains data after completion")
		}
		var row struct {
			Type      string         `json:"type"`
			Format    string         `json:"format"`
			Repo      string         `json:"repo"`
			Analysis  graph.Analysis `json:"analysis"`
			Records   *int           `json:"records"`
			Truncated *bool          `json:"truncated"`
			snapshotSymbol
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return nil, "", fmt.Errorf("invalid snapshot record: %w", err)
		}
		if !metadata && row.Type != "metadata" {
			return nil, "", fmt.Errorf("snapshot must start with metadata")
		}
		switch row.Type {
		case "metadata":
			if metadata || row.Format != "trace.graph/v1" || row.Repo != repo || row.Analysis.Analyzer == "" {
				return nil, "", fmt.Errorf("snapshot format, repository or metadata mismatch")
			}
			metadata = true
			analyzer = row.Analysis.Analyzer
		case "symbol":
			if row.ID == "" || row.File == "" || row.Name == "" || row.Kind == "" || strings.ContainsRune(row.File, '\x00') || strings.ContainsRune(row.Name, '\x00') || strings.ContainsRune(row.Kind, '\x00') || seen[row.ID] {
				return nil, "", fmt.Errorf("snapshot contains an invalid or duplicate symbol")
			}
			seen[row.ID] = true
			symbols = append(symbols, row.snapshotSymbol)
			records++
			if len(symbols) > 100000 {
				return nil, "", fmt.Errorf("snapshot exceeds 100000 symbols")
			}
		case "relation":
			records++
		case "complete":
			if row.Truncated == nil || row.Records == nil || *row.Truncated || *row.Records != records {
				return nil, "", fmt.Errorf("snapshot is truncated or has an invalid record count")
			}
			complete = true
		default:
			return nil, "", fmt.Errorf("unsupported snapshot record type")
		}
	}
	if err := scan.Err(); err != nil {
		return nil, "", err
	}
	if !complete {
		return nil, "", fmt.Errorf("snapshot has no completion marker")
	}
	return symbols, analyzer, nil
}
