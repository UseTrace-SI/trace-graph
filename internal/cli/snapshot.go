package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/spf13/cobra"
)

type snapshotSymbol struct {
	codeSymbol
	SourceHash string `json:"source_hash,omitempty"`
	EndLine    int    `json:"end_line,omitempty"`
}

// Versioned NDJSON export: one consistent read transaction, bounded rows and
// signatures, no source-file reads. A footer distinguishes a finished export
// from interrupted output and explicitly reports a capped export.
func snapshotCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{Use: "snapshot", Short: "Stream a versioned NDJSON graph snapshot", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 1_000_000 {
			return fmt.Errorf("limit must be 1–1000000")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
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
		encoder := json.NewEncoder(cmd.OutOrStdout())
		if err := encoder.Encode(map[string]any{"type": "metadata", "format": "trace.graph/v1", "repo": repoPath, "indexed_at": indexed, "analysis": graph.Capabilities()}); err != nil {
			return err
		}
		count := 0
		truncated := false
		rows, err := tx.QueryContext(ctx, "SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048),COALESCE(c.source_hash,''),COALESCE(c.end_line,s.line) FROM symbols s LEFT JOIN symbol_content c ON c.symbol_id=s.id WHERE s.repo=? ORDER BY s.file,s.line,s.id LIMIT ?", repoPath, limit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			if count >= limit {
				truncated = true
				break
			}
			var s snapshotSymbol
			if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature, &s.SourceHash, &s.EndLine); err != nil {
				rows.Close()
				return err
			}
			if err := encoder.Encode(struct {
				Type string `json:"type"`
				snapshotSymbol
			}{"symbol", s}); err != nil {
				rows.Close()
				return err
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !truncated {
			rows, err = tx.QueryContext(ctx, `SELECT r.id,COALESCE(t.from_id,''),COALESCE(t.to_id,''),COALESCE(t.resolution,'unresolved'),r.from_symbol,r.to_symbol,r.kind,r.file,r.line FROM relations r LEFT JOIN relation_targets t ON t.relation_id=r.id WHERE r.repo=? ORDER BY r.file,r.line,r.id LIMIT ?`, repoPath, limit-count+1)
			if err != nil {
				return err
			}
			for rows.Next() {
				if count >= limit {
					truncated = true
					break
				}
				var id string
				var r codeRelation
				if err := rows.Scan(&id, &r.FromID, &r.ToID, &r.Resolution, &r.From, &r.To, &r.Kind, &r.File, &r.Line); err != nil {
					rows.Close()
					return err
				}
				if err := encoder.Encode(struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					codeRelation
				}{"relation", id, r}); err != nil {
					rows.Close()
					return err
				}
				count++
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return encoder.Encode(map[string]any{"type": "complete", "records": count, "truncated": truncated})
	}}
	cmd.Flags().IntVar(&limit, "limit", 100000, "maximum symbol and relationship records (1–1000000)")
	return cmd
}
