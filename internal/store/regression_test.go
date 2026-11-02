package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestRegressionConcurrentOpenAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	start := make(chan struct{})
	errors := make(chan error, 8)
	var workers sync.WaitGroup
	for i := 0; i < cap(errors); i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			<-start
			db, err := Open(path)
			if err != nil {
				errors <- err
				return
			}
			defer db.Close()
			tx, err := db.Begin()
			if err != nil {
				errors <- err
				return
			}
			defer tx.Rollback()
			var count int
			if err = tx.QueryRow("SELECT count(*) FROM symbols").Scan(&count); err == nil {
				_, err = tx.Exec("INSERT INTO symbols VALUES(?, 'repo', 'main.go', 'Symbol', 'func', 1, '')", fmt.Sprintf("symbol-%d", i))
			}
			if err == nil {
				err = tx.Commit()
			}
			errors <- err
		}(i)
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM symbols").Scan(&count); err != nil || count != 8 {
		t.Fatalf("concurrent symbols lost: count=%d err=%v", count, err)
	}
}

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
