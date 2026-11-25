package cli

import (
	"context"
	"database/sql"

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
