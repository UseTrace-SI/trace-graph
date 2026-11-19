package graph

import (
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// Exec assumes every caller supplies (id, repo, ...). A future query with a
// different shape used to panic on the blind type assertions (or silently
// corrupt the scoped ID via %s formatting); it must instead fail with a
// descriptive, sticky error so the surrounding transaction rolls back cleanly.
func TestIndexWriterExecRejectsMalformedArguments(t *testing.T) {
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

	const query = "INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)"
	cases := []struct {
		name string
		args []any
		want string
	}{
		{"non-string id", []any{123, "repo", "f.go", "A", "func", 1, "func A()"}, "id"},
		{"non-string repo", []any{"id", 456, "f.go", "A", "func", 1, "func A()"}, "repo"},
		{"missing repo", []any{"id"}, "repo"},
		{"no args", nil, "id"},
	}
	for _, tc := range cases {
		w := &indexWriter{tx: tx}
		_, err := w.Exec(query, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
		if _, again := w.Exec(query, "id", "repo", "f.go", "A", "func", 1, "func A()"); again != err {
			t.Fatalf("%s: first error was not preserved: %v", tc.name, again)
		}
	}

	// A well-shaped call still succeeds.
	w := &indexWriter{tx: tx}
	if _, err := w.Exec(query, "id", "repo", "f.go", "A", "func", 1, "func A()"); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
}
