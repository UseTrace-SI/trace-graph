package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestRegressionLegacyIndexUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE symbols(id TEXT PRIMARY KEY,repo TEXT NOT NULL,file TEXT NOT NULL,name TEXT NOT NULL,kind TEXT NOT NULL,line INTEGER NOT NULL,signature TEXT);
CREATE VIRTUAL TABLE symbol_fts USING fts5(name,signature,content='symbols',content_rowid='rowid');
INSERT INTO symbol_fts(rowid,name,signature) VALUES(100,'stale','stale');
INSERT INTO symbols VALUES('legacy','.','gone.go','Old','func',1,'func Old');`)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols").Scan(&n); err != nil || n != 0 {
		t.Fatalf("unsafe old index retained: %d %v", n, err)
	}
	if _, err := db.Exec("INSERT INTO symbols VALUES('new','repo','new.go','New','func',1,'func New')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH 'New'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("reopening cleared the rebuilt index: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH 'stale'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy FTS corruption survived: %d %v", n, err)
	}
}
