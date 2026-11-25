package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/ident"
)

func searchNames(t *testing.T, db *sql.DB, schema int, text string) []string {
	t.Helper()
	search, ok, err := NewSymbolSearch(text, schema)
	if err != nil || !ok {
		t.Fatalf("NewSymbolSearch(%q): %v %v", text, ok, err)
	}
	rows, err := db.Query("SELECT s.name FROM symbols s JOIN symbol_fts ON symbol_fts.rowid=s.rowid WHERE symbol_fts MATCH ? AND s.repo='repo' ORDER BY "+search.OrderBy, append([]any{search.Match}, search.OrderArgs...)...)
	if err != nil {
		t.Fatalf("search %q (%s): %v", text, search.Match, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

var searchFixture = []string{"AnalyzeRepoContext", "analyzeFile", "AnalyzeRepo", "Analyze", "parse_http_request", "HTTPServer", "Unrelated", "reanalyze", "utf8Decode"}

func insertSearchFixture(t *testing.T, db *sql.DB, withTerms bool) {
	t.Helper()
	for i, name := range searchFixture {
		var err error
		if withTerms {
			_, err = db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature,terms) VALUES(?,'repo','f.go',?,'func',?,?,?)", name, name, i+1, "func "+name+"()", ident.Terms(name))
		} else {
			_, err = db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,'repo','f.go',?,'func',?,?)", name, name, i+1, "func "+name+"()")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSymbolSearchMatchesPartialNamesAndRanks(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insertSearchFixture(t, db, true)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		// Exact name, then name prefixes (bm25, then line order).
		{"analyze", []string{"Analyze", "AnalyzeRepoContext", "analyzeFile", "AnalyzeRepo"}},
		{"Repo", []string{"AnalyzeRepo", "AnalyzeRepoContext"}},
		{"analyzeFile", []string{"analyzeFile"}},
		{"analyze_file", []string{"analyzeFile"}},
		{"analyze repo", []string{"AnalyzeRepo", "Analyze", "AnalyzeRepoContext", "analyzeFile"}},
		{"http", []string{"HTTPServer", "parse_http_request"}},
		{"request", []string{"parse_http_request"}},
		{"decode", []string{"utf8Decode"}},
		{"ctx", nil},
	} {
		got := searchNames(t, db, SchemaVersion, tc.query)
		if !sameSet(got, tc.want) || (len(tc.want) > 0 && got[0] != tc.want[0]) {
			t.Errorf("search %q = %q, want %q (first %q)", tc.query, got, tc.want, firstOf(tc.want))
		}
	}
	if _, ok, err := NewSymbolSearch("_ () ", SchemaVersion); ok || err != nil {
		t.Fatalf("query without words: %v %v", ok, err)
	}
	// FTS syntax in the query stays literal.
	if got := searchNames(t, db, SchemaVersion, `NEAR("x" AND) OR * {name}:`); len(got) != 0 {
		t.Fatalf("operators matched: %q", got)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// An index written before schema 4 has no split words. Read-only commands
// do not migrate it, so search falls back to name prefixes; opening it for
// writing adds the words without discarding the indexed generation.
func TestSearchTermsMigrationKeepsIndexAndLegacyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE symbols (id TEXT PRIMARY KEY, repo TEXT NOT NULL, file TEXT NOT NULL,
  name TEXT NOT NULL, kind TEXT NOT NULL, line INTEGER NOT NULL, signature TEXT);
CREATE VIRTUAL TABLE symbol_fts USING fts5(name, signature, content='symbols', content_rowid='rowid');
CREATE TRIGGER symbols_ai AFTER INSERT ON symbols BEGIN
  INSERT INTO symbol_fts(rowid,name,signature) VALUES(new.rowid,new.name,new.signature);
END;
CREATE TRIGGER symbols_ad AFTER DELETE ON symbols BEGIN
  INSERT INTO symbol_fts(symbol_fts,rowid,name,signature) VALUES('delete',old.rowid,old.name,old.signature);
END;
PRAGMA user_version=3;`)
	if err != nil {
		t.Fatal(err)
	}
	insertSearchFixture(t, legacy, false)
	if got := searchNames(t, legacy, 3, "analyze"); !sameSet(got, []string{"Analyze", "AnalyzeRepoContext", "analyzeFile", "AnalyzeRepo"}) || got[0] != "Analyze" {
		t.Fatalf("legacy prefix search: %q", got)
	}
	legacy.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, symbols int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("user_version %d %v", version, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE terms IS NOT NULL").Scan(&symbols); err != nil || symbols != len(searchFixture) {
		t.Fatalf("migration dropped or left symbols without words: %d %v", symbols, err)
	}
	if got := searchNames(t, db, SchemaVersion, "repo"); !reflect.DeepEqual(sortedCopy(got), []string{"AnalyzeRepo", "AnalyzeRepoContext"}) {
		t.Fatalf("migrated split-word search: %q", got)
	}
	// The rebuilt triggers keep the index consistent through deletes.
	if _, err := db.Exec("DELETE FROM symbols WHERE name='AnalyzeRepo'"); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, db, SchemaVersion, "repo"); !reflect.DeepEqual(got, []string{"AnalyzeRepoContext"}) {
		t.Fatalf("deleted symbol still found: %q", got)
	}
	if _, err := db.Exec("INSERT INTO symbol_fts(symbol_fts) VALUES('integrity-check')"); err != nil {
		t.Fatalf("search index inconsistent: %v", err)
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Schema 5 embeds split identifiers and file paths, so vectors written by
// an older analyzer are dropped and recomputed by the next analysis.
func TestEmbeddingSchemaUpgradeClearsOutdatedVectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature,terms) VALUES('s','repo','f.go','A','func',1,'func A()','A'); INSERT INTO symbol_embeddings(symbol_id,dim,vector) VALUES('s',1,x'00000000'); PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var vectors, symbols int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM symbol_embeddings),(SELECT count(*) FROM symbols)").Scan(&vectors, &symbols); err != nil || vectors != 0 || symbols != 1 {
		t.Fatalf("vectors=%d symbols=%d %v", vectors, symbols, err)
	}
}

func TestOrphanGoDirectoryTableIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE repo_go_dirs(repo TEXT PRIMARY KEY,digest TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='repo_go_dirs'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan table kept: %d %v", n, err)
	}
}
