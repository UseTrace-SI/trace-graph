package cli

import (
	"context"
	"database/sql"
)

// sqlLimit turns a limit below 1 into SQLite's unbounded LIMIT -1.
func sqlLimit(limit int) int {
	if limit < 1 {
		return -1
	}
	return limit
}

// lookupDefinitions lists the definitions of a symbol name in file and line
// order. A limit below 1 is unbounded.
func lookupDefinitions(ctx context.Context, db *sql.DB, repo, name string, limit int) ([]codeSymbol, error) {
	rows, err := db.QueryContext(ctx, "SELECT id,file,name,kind,line,substr(COALESCE(signature,''),1,2048) FROM symbols WHERE name=? AND repo=? ORDER BY file,line,id LIMIT ?", name, repo, sqlLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	defs := []codeSymbol{}
	for rows.Next() {
		var s codeSymbol
		if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature); err != nil {
			return nil, err
		}
		defs = append(defs, s)
	}
	return defs, rows.Err()
}

// callerRef is a call or dispatch site that reaches a symbol.
type callerRef struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
	Line   int    `json:"line"`
}

type neighborsResult struct {
	Definitions []codeSymbol `json:"definitions"`
	Callers     []callerRef  `json:"callers"`
}

// lookupNeighbors lists the definitions of a symbol name or ID and the
// direct callers that reach them through resolved call and dispatch edges,
// or through unresolved calls of the same name. A limit below 1 is
// unbounded; it applies to each list.
func lookupNeighbors(ctx context.Context, db *sql.DB, repo, selector string, limit int) (neighborsResult, error) {
	result := neighborsResult{Definitions: []codeSymbol{}, Callers: []callerRef{}}
	rows, err := db.QueryContext(ctx, "SELECT id,file,name,kind,line,substr(COALESCE(signature,''),1,2048) FROM symbols WHERE (name=? OR id=?) AND repo=? ORDER BY file,line,id LIMIT ?", selector, selector, repo, sqlLimit(limit))
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var s codeSymbol
		if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature); err != nil {
			rows.Close()
			return result, err
		}
		result.Definitions = append(result.Definitions, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = db.QueryContext(ctx, `SELECT DISTINCT r.from_symbol,r.file,r.line FROM relations r LEFT JOIN relation_targets t ON t.relation_id=r.id
 WHERE r.repo=? AND r.kind IN ('call','dispatch') AND (t.to_id IN (SELECT id FROM symbols WHERE repo=? AND (name=? OR id=?)) OR (t.relation_id IS NULL AND r.to_symbol=?)) ORDER BY r.file,r.line LIMIT ?`, repo, repo, selector, selector, selector, sqlLimit(limit))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var c callerRef
		if err := rows.Scan(&c.Symbol, &c.File, &c.Line); err != nil {
			return result, err
		}
		result.Callers = append(result.Callers, c)
	}
	return result, rows.Err()
}
