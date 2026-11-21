//go:build !treesitter

package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// Generated Go (goyacc, quicktemplate, ...) carries //line directives that
// map positions to another file. Symbols and calls used the remapped lines,
// columns after a "file:line" directive became 0, so two calls on one line
// shared a relation ID and one was dropped, and two same-name methods mapped
// to one template line collided and failed the whole index.
func TestRegressionLineDirectivesKeepPhysicalPositions(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "package p\n" + // 1
		"type T struct{}\n" + // 2
		"type U struct{}\n" + // 3
		"func Target(){}\n" + // 4
		"//line generated.y:10\n" + // 5
		"func A(){ Target(); Target() }\n" + // 6
		"//line generated.y:10\n" + // 7
		"func (T) M(){}\n" + // 8
		"//line generated.y:10\n" + // 9
		"func (U) M(){}\n" // 10
	if err := os.WriteFile(filepath.Join(root, "gen.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatalf("line directives broke indexing: %v", err)
	}
	rows, err := db.Query("SELECT s.name,s.line,c.end_line FROM symbols s JOIN symbol_content c ON c.symbol_id=s.id WHERE s.repo=? AND s.kind IN ('func','method') ORDER BY s.line", root)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type position struct {
		name      string
		line, end int
	}
	var got []position
	for rows.Next() {
		var p position
		if err := rows.Scan(&p.name, &p.line, &p.end); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []position{{"Target", 4, 4}, {"A", 6, 6}, {"M", 8, 8}, {"M", 10, 10}}
	if len(got) != len(want) {
		t.Fatalf("symbols %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("symbols %+v, want %+v", got, want)
		}
	}
	var calls int
	if err := db.QueryRow("SELECT count(*) FROM relations WHERE repo=? AND from_symbol='A' AND to_symbol='Target' AND line=6", root).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("calls on one generated line were merged: %d %v", calls, err)
	}
}
