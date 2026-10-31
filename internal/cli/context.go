package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/UseTrace-SI/trace-graph/internal/embed"
	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/spf13/cobra"
)

func refreshCmd() *cobra.Command {
	return &cobra.Command{Use: "refresh", Short: "Index the repository and return protocol-1 JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()
		count, err := graph.AnalyzeRepo(repoPath, db)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"protocol": 1, "status": "ready", "symbols_updated": count})
	}}
}

type codeContext struct {
	Protocol  int            `json:"protocol"`
	Status    string         `json:"status"`
	Repo      string         `json:"repo"`
	IndexedAt int64          `json:"indexed_at,omitempty"`
	Symbols   []codeSymbol   `json:"symbols"`
	Relations []codeRelation `json:"relations"`
}

type codeSymbol struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Line      int    `json:"line"`
	Signature string `json:"signature"`
}

type codeRelation struct {
	From string `json:"from_symbol"`
	To   string `json:"to_symbol"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
}

func contextCmd() *cobra.Command {
	var path, query string
	var limit int
	var semantic bool
	cmd := &cobra.Command{Use: "context", Short: "Read bounded code context as protocol-1 JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if len(path) > 4096 || len(query) > 4096 {
			return fmt.Errorf("context path or query exceeds 4096 bytes")
		}
		out := codeContext{Protocol: 1, Status: "missing_index", Repo: repoPath, Symbols: []codeSymbol{}, Relations: []codeRelation{}}
		encode := func() error { return json.NewEncoder(cmd.OutOrStdout()).Encode(out) }
		if path != "" {
			path = filepath.ToSlash(filepath.Clean(path))
			if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
				return fmt.Errorf("path must be relative to the repository")
			}
		}
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			return encode()
		} else if err != nil {
			return err
		}
		abs, err := filepath.Abs(dbPath)
		if err != nil {
			return err
		}
		u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return err
		}
		defer db.Close()
		var exists int
		if err := db.QueryRowContext(cmd.Context(), "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='repo_state'").Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			out.Status = "needs_refresh"
			return encode()
		}
		if err := db.QueryRowContext(cmd.Context(), "SELECT indexed_at FROM repo_state WHERE repo=?", repoPath).Scan(&out.IndexedAt); err == sql.ErrNoRows {
			return encode()
		} else if err != nil {
			return err
		}
		if limit <= 0 || limit > 50 {
			limit = 20
		}
		if semantic && strings.TrimSpace(query) != "" {
			q := `SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048),e.vector FROM symbols s LEFT JOIN symbol_embeddings e ON e.symbol_id=s.id WHERE s.repo=?`
			args := []any{repoPath}
			if path != "" {
				q += ` AND s.file=?`
				args = append(args, path)
			}
			rows, err := db.QueryContext(cmd.Context(), q, args...)
			if err != nil {
				return err
			}
			type scored struct {
				item  codeSymbol
				score float64
			}
			qv := embed.Embed(query)
			var cands []scored
			for rows.Next() {
				var item codeSymbol
				var vec []byte
				if err := rows.Scan(&item.ID, &item.File, &item.Name, &item.Kind, &item.Line, &item.Signature, &vec); err != nil {
					rows.Close()
					return err
				}
				score := 0.0
				if v := embed.Decode(vec, embed.Dim); v != nil {
					score = embed.Cosine(qv, v)
				}
				cands = append(cands, scored{item: item, score: score})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for i := 1; i < len(cands); i++ {
				for j := i; j > 0 && cands[j-1].score < cands[j].score; j-- {
					cands[j-1], cands[j] = cands[j], cands[j-1]
				}
			}
			if limit > len(cands) {
				limit = len(cands)
			}
			for i := 0; i < limit; i++ {
				out.Symbols = append(out.Symbols, cands[i].item)
			}
		} else {
			q := `SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048) FROM symbols s`
			var args []any
			words := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
			if len(words) > 16 {
				words = words[:16]
			}
			for i := range words {
				words[i] = `"` + words[i] + `"`
			}
			if path == "" && len(words) > 0 {
				q += ` JOIN symbol_fts f ON f.rowid=s.rowid WHERE symbol_fts MATCH ? AND s.repo=?`
				args = append(args, strings.Join(words, " OR "), repoPath)
			} else {
				q += ` WHERE s.repo=?`
				args = append(args, repoPath)
			}
			if path != "" {
				q += ` AND s.file=?`
				args = append(args, path)
			}
			q += ` ORDER BY s.file,s.line,s.id LIMIT ?`
			args = append(args, limit)
			rows, err := db.QueryContext(cmd.Context(), q, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var item codeSymbol
				if err := rows.Scan(&item.ID, &item.File, &item.Name, &item.Kind, &item.Line, &item.Signature); err != nil {
					rows.Close()
					return err
				}
				out.Symbols = append(out.Symbols, item)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		// Include callers and outgoing relationships for the selected symbols.
		if len(out.Symbols) > 0 {
			var placeholders []string
			args := []any{repoPath}
			for _, s := range out.Symbols {
				placeholders = append(placeholders, "?")
				args = append(args, s.Name)
			}
			for _, s := range out.Symbols {
				args = append(args, s.Name)
			}
			in := strings.Join(placeholders, ",")
			rows, err := db.QueryContext(cmd.Context(), `SELECT from_symbol,to_symbol,kind,file,line FROM relations WHERE repo=? AND (from_symbol IN (`+in+`) OR to_symbol IN (`+in+`)) ORDER BY file,line,id LIMIT 50`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item codeRelation
				if err := rows.Scan(&item.From, &item.To, &item.Kind, &item.File, &item.Line); err != nil {
					return err
				}
				out.Relations = append(out.Relations, item)
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		out.Status = "ready"
		return encode()
	}}
	cmd.Flags().StringVar(&path, "path", "", "repository-relative file")
	cmd.Flags().StringVar(&query, "query", "", "symbol words (used when path is empty)")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum symbols (1-50)")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "rank symbols by local embedding similarity")
	return cmd
}
