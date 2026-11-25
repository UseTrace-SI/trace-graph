package cli

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/UseTrace-SI/trace-graph/internal/embed"
	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// indexSchema reads the schema version of an index that read-only commands
// open without migrating it.
func indexSchema(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}

// searchSymbols ranks repository symbols for literal query words (see
// store.NewSymbolSearch), optionally within one file. A limit below 1 is
// unbounded. ok is false when the query has no words.
func searchSymbols(ctx context.Context, db *sql.DB, repo, text, path string, limit int) (symbols []codeSymbol, ok bool, err error) {
	schema, err := indexSchema(ctx, db)
	if err != nil {
		return nil, false, err
	}
	search, ok, err := store.NewSymbolSearch(text, schema)
	if err != nil || !ok {
		return nil, ok, err
	}
	q := `SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048) FROM symbols s JOIN symbol_fts ON symbol_fts.rowid=s.rowid WHERE symbol_fts MATCH ? AND s.repo=?`
	args := []any{search.Match, repo}
	if path != "" {
		q += ` AND s.file=?`
		args = append(args, path)
	}
	if limit < 1 {
		limit = -1
	}
	q += ` ORDER BY ` + search.OrderBy + ` LIMIT ?`
	args = append(append(args, search.OrderArgs...), limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	symbols = []codeSymbol{}
	for rows.Next() {
		var item codeSymbol
		if err := rows.Scan(&item.ID, &item.File, &item.Name, &item.Kind, &item.Line, &item.Signature); err != nil {
			return nil, true, err
		}
		symbols = append(symbols, item)
	}
	return symbols, true, rows.Err()
}

// semanticCandidates bounds the symbols reranked by embedding similarity.
const semanticCandidates = 500

// semanticSymbols reranks a bounded candidate set by local embedding
// similarity: the best ranked search matches of the query words plus
// symbols in files whose path contains a query word. Candidates are
// restricted to path when it is set. Symbols without a positive similarity
// are dropped; ties keep file and line order. When the deadline expires,
// the best candidates scored so far are returned.
func semanticSymbols(ctx context.Context, db *sql.DB, repo, query, path string, limit int) ([]codeSymbol, error) {
	candidates, _, err := searchSymbols(ctx, db, repo, query, path, semanticCandidates)
	if err != nil {
		if ctx.Err() != nil {
			return []codeSymbol{}, nil
		}
		return nil, err
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.ID] = true
	}
	words, err := store.QueryWords(query)
	if err != nil {
		return nil, err
	}
	if len(words) > 0 && ctx.Err() == nil {
		var match []string
		args := []any{repo}
		for _, word := range words {
			match = append(match, "instr(lower(s.file),lower(?))>0")
			args = append(args, word)
		}
		q := `SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,2048) FROM symbols s WHERE s.repo=? AND (` + strings.Join(match, " OR ") + `)`
		if path != "" {
			q += ` AND s.file=?`
			args = append(args, path)
		}
		q += ` ORDER BY s.file,s.line,s.id LIMIT ?`
		args = append(args, semanticCandidates)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item codeSymbol
			if err := rows.Scan(&item.ID, &item.File, &item.Name, &item.Kind, &item.Line, &item.Signature); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[item.ID] {
				seen[item.ID] = true
				candidates = append(candidates, item)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
	}
	qv := embed.Embed(query)
	type scored struct {
		item  codeSymbol
		score float64
	}
	var ranked []scored
	const chunk = 200
	for start := 0; start < len(candidates) && ctx.Err() == nil; start += chunk {
		batch := candidates[start:min(start+chunk, len(candidates))]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		byID := map[string]codeSymbol{}
		for i, c := range batch {
			placeholders[i], args[i] = "?", c.ID
			byID[c.ID] = c
		}
		rows, err := db.QueryContext(ctx, `SELECT symbol_id,vector FROM symbol_embeddings WHERE symbol_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return nil, err
		}
		for rows.Next() {
			var id string
			var vector []byte
			if err := rows.Scan(&id, &vector); err != nil {
				rows.Close()
				return nil, err
			}
			if v := embed.Decode(vector, embed.Dim); v != nil {
				if score := embed.Cosine(qv, v); score > 0 {
					ranked = append(ranked, scored{item: byID[id], score: score})
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.item.File != b.item.File {
			return a.item.File < b.item.File
		}
		if a.item.Line != b.item.Line {
			return a.item.Line < b.item.Line
		}
		return a.item.ID < b.item.ID
	})
	symbols := []codeSymbol{}
	for i := 0; i < len(ranked) && i < limit; i++ {
		symbols = append(symbols, ranked[i].item)
	}
	return symbols, nil
}
