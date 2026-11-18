package store

import (
	"strings"
	"testing"
)

func TestLiteralQuery(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"empty", "", `""`},
		{"punctuation", "foo(bar)!", `"foo" OR "bar"`},
		{"operators are literal", "AND OR NOT", `"AND" OR "OR" OR "NOT"`},
		{"underscore retained", "some_name", `"some_name"`},
		{"words trimmed at sixteen", strings.Repeat("w ", 20), strings.TrimRight(strings.Repeat(`"w" OR `, 16), " OR")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LiteralQuery(tc.text)
			if err != nil || got != tc.want {
				t.Fatalf("LiteralQuery(%q) = %q, %v; want %q", tc.text, got, err, tc.want)
			}
		})
	}
	if _, err := LiteralQuery(strings.Repeat("x", 4097)); err == nil {
		t.Fatal("oversized query accepted")
	}
}

// The quoted words must be usable as an FTS MATCH expression without being
// interpreted as FTS operators.
func TestLiteralQueryMatchesFTS(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO symbols VALUES('s','repo','main.go','Alpha','func',1,'func Alpha')"); err != nil {
		t.Fatal(err)
	}
	q, err := LiteralQuery("Alpha() AND missing")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols s JOIN symbol_fts f ON f.rowid=s.rowid WHERE symbol_fts MATCH ? AND s.repo=?", q, "repo").Scan(&n); err != nil || n != 1 {
		t.Fatalf("literal FTS query failed: %d %v", n, err)
	}
}
