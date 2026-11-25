package graph

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// A strict batch (revision comparison) that records a colliding identity
// keeps the collision as its error, ignores later observations and is never
// partially written.
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
	b.strict = true
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

// A persistent batch keeps the first of two colliding declarations and drops
// the later one with the observations recorded for it.
func TestFileBatchDropsLaterCollidingDeclaration(t *testing.T) {
	b := (&indexWriter{}).batch("f.go")
	b.symbol("f.go:A:1", "A", "func", 1, "func A()")
	b.content("f.go:A:1", []byte("first"), 1)
	b.call("f.go:A:1", "A", "First", 1, 5, "identifier")
	b.symbol("f.go:A:1", "A", "method", 1, "func (U) A()")
	b.content("f.go:A:1", []byte("second"), 1)
	b.receiverOf("f.go:A:1", "U")
	b.call("f.go:A:1", "A", "Second", 1, 20, "identifier")
	b.relation("f.go#import:1:1", "", "f.go", "x", "import", 1, "hint")
	b.symbol("f.go:B:2", "B", "func", 2, "func B()")
	b.call("f.go:B:2", "B", "Third", 2, 5, "identifier")
	if b.err != nil || len(b.collisions) != 1 || b.collisions[0] != "f.go:A:1" {
		t.Fatalf("collision handling: %v %v", b.err, b.collisions)
	}
	var kept []string
	for _, o := range b.rows {
		switch o.kind {
		case observeSymbol:
			kept = append(kept, "symbol:"+o.id+":"+o.symbolKind)
		case observeContent:
			kept = append(kept, "content:"+o.id)
		case observeReceiver:
			kept = append(kept, "receiver:"+o.id)
		case observeRelation:
			kept = append(kept, "relation:"+o.toName)
		}
	}
	want := "symbol:f.go:A:1:func content:f.go:A:1 relation:First relation:x symbol:f.go:B:2:func relation:Third"
	if got := strings.Join(kept, " "); got != want {
		t.Fatalf("kept %s\nwant %s", got, want)
	}
	if w := b.collisionWarning(); !strings.Contains(w, "f.go:A:1") || !strings.Contains(w, "dropped 1") {
		t.Fatalf("warning: %s", w)
	}
}

// A one-line overload signature and its implementation share an identity;
// the implementation is kept whichever comes first.
func TestFileBatchPrefersDeclarationWithBody(t *testing.T) {
	kept := func(b *fileBatch) string {
		var out []string
		for _, o := range b.rows {
			switch o.kind {
			case observeSymbol:
				out = append(out, "symbol:"+o.signature)
			case observeContent:
				out = append(out, "content:"+o.hash[:4])
			case observeRelation:
				out = append(out, "call:"+o.toName)
			}
		}
		return strings.Join(out, " ")
	}
	hash := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:4] }

	b := (&indexWriter{}).batch("a.ts")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "signature", false, "")
	b.content("a.ts:f:1", []byte("sig"), 1)
	b.symbolBody("a.ts:f:1", "f", "func", 1, "implementation", true, "")
	b.content("a.ts:f:1", []byte("impl"), 1)
	b.call("a.ts:f:1", "f", "g", 1, 40, "unresolved")
	b.symbolBody("a.ts:g:2", "g", "func", 2, "g", true, "")
	if got, want := kept(b), "symbol:implementation content:"+hash("impl")+" call:g symbol:g"; got != want {
		t.Fatalf("signature first: kept %s, want %s", got, want)
	}
	if len(b.collisions) != 1 || b.err != nil {
		t.Fatalf("collision not recorded: %v %v", b.collisions, b.err)
	}

	b = (&indexWriter{}).batch("a.ts")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "implementation", true, "")
	b.content("a.ts:f:1", []byte("impl"), 1)
	b.call("a.ts:f:1", "f", "g", 1, 40, "unresolved")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "signature", false, "")
	b.content("a.ts:f:1", []byte("sig"), 1)
	if got, want := kept(b), "symbol:implementation content:"+hash("impl")+" call:g"; got != want {
		t.Fatalf("implementation first: kept %s, want %s", got, want)
	}

	// Without body information the first declaration is kept.
	b = (&indexWriter{}).batch("a.ts")
	b.symbol("a.ts:f:1", "f", "func", 1, "first")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "second", true, "")
	if got := kept(b); got != "symbol:first" {
		t.Fatalf("unknown body: kept %s", got)
	}

	// A declaration replaces a function expression recorded before it; a
	// signature that replaced one is in turn replaced by its implementation.
	b = (&indexWriter{}).batch("a.ts")
	b.expression("a.ts:f:1", "f", "func", 1, "expression")
	b.content("a.ts:f:1", []byte("expr"), 1)
	b.call("a.ts:f:1", "f", "wrong", 1, 10, "unresolved")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "signature", false, "")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "implementation", true, "")
	b.content("a.ts:f:1", []byte("impl"), 1)
	b.call("a.ts:f:1", "f", "right", 1, 40, "unresolved")
	b.expression("a.ts:f:1", "f", "func", 1, "late expression")
	b.call("a.ts:f:1", "f", "late", 1, 60, "unresolved")
	if got, want := kept(b), "symbol:implementation content:"+hash("impl")+" call:right"; got != want {
		t.Fatalf("expression first: kept %s, want %s", got, want)
	}
	if len(b.collisions) != 3 {
		t.Fatalf("collisions: %v", b.collisions)
	}
	// A bodyless member signature (`interface I { f(): void }`) never
	// replaces a function expression; only a declaration with a body does.
	b = (&indexWriter{}).batch("a.ts")
	b.expression("a.ts:f:1", "f", "func", 1, "expression")
	b.call("a.ts:f:1", "f", "kept", 1, 10, "unresolved")
	b.symbolBody("a.ts:f:1", "f", "method", 1, "member signature", false, "")
	if got := kept(b); got != "symbol:expression call:kept" {
		t.Fatalf("member signature after expression: kept %s", got)
	}
	// Two expressions: the first is kept.
	b = (&indexWriter{}).batch("a.ts")
	b.expression("a.ts:f:1", "f", "func", 1, "first")
	b.expression("a.ts:f:1", "f", "func", 1, "second")
	if got := kept(b); got != "symbol:first" {
		t.Fatalf("two expressions: kept %s", got)
	}

	// Dropped rows are never stored.
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
	b = w.batch("a.ts")
	b.symbolBody("a.ts:f:1", "f", "func", 1, "signature", false, "")
	b.content("a.ts:f:1", []byte("sig"), 1)
	b.symbolBody("a.ts:f:1", "f", "func", 1, "implementation", true, "")
	b.content("a.ts:f:1", []byte("impl"), 1)
	if err := w.flush("repo", b); err != nil {
		t.Fatal(err)
	}
	var signature, stored string
	if err := tx.QueryRow("SELECT s.signature,c.source_hash FROM symbols s JOIN symbol_content c ON c.symbol_id=s.id").Scan(&signature, &stored); err != nil || signature != "implementation" || stored[:4] != hash("impl") {
		t.Fatalf("stored %q %q %v", signature, stored, err)
	}
}
