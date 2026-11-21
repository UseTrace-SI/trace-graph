//go:build !treesitter

package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// Masked comments must not affect a lexical declaration's kind, signature or
// source hash. A trailing "// class marker" used to misclassify an arrow
// function as a type.
func TestLexicalKindIgnoresCommentEmbeddedKeywords(t *testing.T) {
	// AnalyzeRepoContext indexes under the canonical repository path. t.TempDir()
	// is a symlinked /var path on macOS, making repo-scoped queries fail there.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "export const Foo = (x) => x // class marker\n" +
		"export const Bar = (x) => x /* type trap */\n" +
		"class Real {}\n" +
		"function RealFn() {}\n"
	if err := os.WriteFile(filepath.Join(root, "index.mts"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"Foo": "func", "Bar": "func", "Real": "type", "RealFn": "func"} {
		var got string
		if err := db.QueryRow("SELECT kind FROM symbols WHERE repo=? AND name=?", root, name).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: kind %q want %q (%v)", name, got, want, err)
		}
	}
	var signature string
	if err := db.QueryRow("SELECT signature FROM symbols WHERE repo=? AND name='Foo'", root).Scan(&signature); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(signature, "class") || strings.Contains(signature, "marker") {
		t.Fatalf("comment leaked into signature: %q", signature)
	}
	var hash string
	if err := db.QueryRow("SELECT c.source_hash FROM symbol_content c JOIN symbols s ON s.id=c.symbol_id WHERE s.repo=? AND s.name='Foo'", root).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Fatalf("unexpected hash: %q", hash)
	}
	// A comment-only edit must not change the hash computed from the masked line.
	changed := "export const Foo = (x) => x // interface marker\n" +
		"export const Bar = (x) => x /* trait trap */\n" +
		"class Real {}\n" +
		"function RealFn() {}\n"
	if err := os.WriteFile(filepath.Join(root, "index.mts"), []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := db.QueryRow("SELECT c.source_hash FROM symbol_content c JOIN symbols s ON s.id=c.symbol_id WHERE s.repo=? AND s.name='Foo'", root).Scan(&after); err != nil || after != hash {
		t.Fatalf("comment edit changed source hash: %q != %q %v", after, hash, err)
	}
}

// A Rust lifetime's quote used to open a "string" that masked every following
// line up to the next quote, so declarations after `<'a>` vanished.
func TestRegressionRustLifetimesDoNotMaskLaterDeclarations(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "pub fn first<'a>(x: &'a str) -> &'a str { x }\n" +
		"pub fn second() {}\n" +
		"struct Holder<'a> { v: &'a str }\n" +
		"fn third() -> char { 'x' }\n" +
		"fn quote() -> char { '\\'' }\n" +
		"fn labelled() { 'outer: loop { break 'outer; } }\n" +
		"const TEXT: &str = \"fn fake_in_string() {}\";\n" +
		"fn last() -> &'static str { \"done\" }\n"
	if err := os.WriteFile(filepath.Join(root, "lib.rs"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"first": 1, "second": 1, "Holder": 1, "third": 1, "quote": 1, "labelled": 1, "last": 1, "fake_in_string": 0} {
		var got int
		if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=? AND name=?", root, name).Scan(&got); err != nil || got != want {
			t.Errorf("%s: %d symbols, want %d (%v)", name, got, want, err)
		}
	}
}

func TestRustCharLiteralWidth(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{"'a'", 3}, {"'é'", 4}, {`'\n'`, 4}, {`'\''`, 4}, {`'\u{1F600}'`, 11},
		{"'a>", 0}, {"'static", 0}, {"'outer:", 0}, {"'", 0}, {"'\n'", 0},
	} {
		if got := rustCharLiteral([]byte(tc.src), 0); got != tc.want {
			t.Errorf("rustCharLiteral(%q) = %d, want %d", tc.src, got, tc.want)
		}
	}
}
