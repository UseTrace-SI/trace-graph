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
	if _, err := db.Exec("PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL"); err != nil {
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
CREATE INDEX IF NOT EXISTS symbols_repo_file ON symbols(repo,file,line);
CREATE INDEX IF NOT EXISTS symbols_repo_name ON symbols(repo,name);
CREATE TABLE IF NOT EXISTS file_cache (path TEXT PRIMARY KEY, mtime INTEGER NOT NULL, size INTEGER NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(name, signature, content='symbols', content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS symbols_ai AFTER INSERT ON symbols BEGIN
  INSERT INTO symbol_fts(rowid,name,signature) VALUES(new.rowid,new.name,new.signature);
END;
CREATE TRIGGER IF NOT EXISTS symbols_ad AFTER DELETE ON symbols BEGIN
  INSERT INTO symbol_fts(symbol_fts,rowid,name,signature) VALUES('delete',old.rowid,old.name,old.signature);
END;
CREATE TRIGGER IF NOT EXISTS symbols_au AFTER UPDATE ON symbols BEGIN
  INSERT INTO symbol_fts(symbol_fts,rowid,name,signature) VALUES('delete',old.rowid,old.name,old.signature);
  INSERT INTO symbol_fts(rowid,name,signature) VALUES(new.rowid,new.name,new.signature);
END;
CREATE TABLE IF NOT EXISTS file_cache_v2 (repo TEXT NOT NULL, path TEXT NOT NULL, digest TEXT NOT NULL, PRIMARY KEY(repo,path));
CREATE TABLE IF NOT EXISTS repo_state (repo TEXT PRIMARY KEY, indexed_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS symbol_embeddings (symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,
  dim INTEGER NOT NULL, vector BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS relations (id TEXT PRIMARY KEY, repo TEXT NOT NULL, from_symbol TEXT NOT NULL,
  to_symbol TEXT NOT NULL, kind TEXT NOT NULL, file TEXT NOT NULL, line INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS relations_from ON relations(from_symbol);
CREATE INDEX IF NOT EXISTS relations_to ON relations(to_symbol);
CREATE INDEX IF NOT EXISTS relations_repo_from ON relations(repo,from_symbol);
CREATE INDEX IF NOT EXISTS relations_repo_to ON relations(repo,to_symbol);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		// The old derived index had unscoped IDs and potentially stale FTS
		// entries. Rebuild it once rather than carrying corruption forward.
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec("INSERT INTO symbol_fts(symbol_fts) VALUES('rebuild'); DELETE FROM symbols; DELETE FROM relations; DELETE FROM file_cache; DELETE FROM file_cache_v2; PRAGMA user_version=1"); err != nil {
			return err
		}
		return tx.Commit()
	}
	return nil
}
