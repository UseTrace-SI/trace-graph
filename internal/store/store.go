package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS symbols (id TEXT PRIMARY KEY, repo TEXT NOT NULL, file TEXT NOT NULL,
  name TEXT NOT NULL, kind TEXT NOT NULL, line INTEGER NOT NULL, signature TEXT);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file);
CREATE TABLE IF NOT EXISTS file_cache (path TEXT PRIMARY KEY, mtime INTEGER NOT NULL, size INTEGER NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(name, signature, content='symbols', content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS symbols_ai AFTER INSERT ON symbols BEGIN
  INSERT INTO symbol_fts(rowid,name,signature) VALUES(new.rowid,new.name,new.signature);
END;
CREATE TABLE IF NOT EXISTS relations (id TEXT PRIMARY KEY, repo TEXT NOT NULL, from_symbol TEXT NOT NULL,
  to_symbol TEXT NOT NULL, kind TEXT NOT NULL, file TEXT NOT NULL, line INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS relations_from ON relations(from_symbol);
CREATE INDEX IF NOT EXISTS relations_to ON relations(to_symbol);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}

