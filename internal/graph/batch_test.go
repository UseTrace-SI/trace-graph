package graph

import (
	"errors"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// A batch that records a colliding identity keeps the collision as its error,
// ignores later observations and is never partially written.
func TestFileBatchCollisionWritesNothing(t *testing.T) {
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
	w := &indexWriter{tx: tx}
	defer w.close()

	b := w.batch("f.go")
	b.symbol("f.go:A:1", "A", "func", 1, "func A()")
	b.content("f.go:A:1", []byte("func A() {}"), 1)
	b.symbol("f.go:A:1", "A", "func", 1, "func A()")
	b.call("f.go:A:1", "A", "B", 1, 10, "identifier")
	var collision *collisionError
	if !errors.As(b.err, &collision) || collision.id != "f.go:A:1" {
		t.Fatalf("collision not recorded: %v", b.err)
	}
	if len(b.rows) != 2 {
		t.Fatalf("observations after the collision were recorded: %d", len(b.rows))
	}
	if err := w.flush("repo", b); !errors.As(err, &collision) {
		t.Fatalf("flush did not return the collision: %v", err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM symbols").Scan(&n); err != nil || n != 0 {
		t.Fatalf("collision batch wrote %d symbols: %v", n, err)
	}
	// The writer itself is still usable for the next file.
	ok := w.batch("g.go")
	ok.namespace = "ns"
	ok.symbol("g.go:G:1", "G", "func", 1, "func G()")
	ok.receiverOf("g.go:G:1", "T")
	ok.relation("g.go#import:1:1", "", "g.go", "x", "import", 1, "hint")
	if err := w.flush("repo", ok); err != nil {
		t.Fatal(err)
	}
	var id, namespace, receiver, from string
	if err := tx.QueryRow("SELECT s.id,sc.namespace,r.receiver FROM symbols s JOIN symbol_scopes sc ON sc.symbol_id=s.id JOIN symbol_receivers r ON r.symbol_id=s.id").Scan(&id, &namespace, &receiver); err != nil {
		t.Fatal(err)
	}
	if id != scopedID("repo", "g.go:G:1") || namespace != "ns" || receiver != "T" {
		t.Fatalf("typed rows stored wrongly: %q %q %q", id, namespace, receiver)
	}
	if err := tx.QueryRow("SELECT from_id FROM relation_targets").Scan(&from); err != nil || from != "" {
		t.Fatalf("module-level relation has a source: %q %v", from, err)
	}
	if w.records != 5 {
		t.Fatalf("counted %d rows, want 5", w.records)
	}
}
