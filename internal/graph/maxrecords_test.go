package graph

import (
	"fmt"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// The observation cap must bound the rows actually written, not only symbol
// and relation inserts. Scopes, content and targets used to bypass it.
func TestMaxRecordsBoundsActualRows(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	src := "package p\n"
	for i := 0; i < 20; i++ {
		src += fmt.Sprintf("func F%d(){ F%d() }\n", i, (i+1)%20)
	}
	const cap = 7
	w := &indexWriter{tx: tx, maxRecords: cap}
	defer w.close()
	batch := w.batch("p.go")
	if _, err := analyzeFile("repo", "p.go", "p.go", []byte(src), batch); err != nil {
		t.Fatal(err)
	}
	if err := w.flush("repo", batch); err == nil || err != w.err || w.records != cap {
		t.Fatalf("cap not enforced at exactly %d rows: records=%d err=%v", cap, w.records, w.err)
	}
	var total int
	for _, table := range []string{"symbols", "symbol_scopes", "symbol_content", "relations", "relation_targets", "symbol_receivers"} {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total == 0 || total > cap {
		t.Fatalf("row budget exceeded: %d rows for cap %d", total, cap)
	}
	// All writes must remain in the same transaction so a failed generation
	// can be rolled back atomically.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.QueryRow("SELECT count(*) FROM symbols").Scan(&after); err != nil || after != 0 {
		t.Fatalf("failed generation left rows: %d %v", after, err)
	}
}
