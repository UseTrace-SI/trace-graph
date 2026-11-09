package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	// Imports read fingerprints before writing. Reserve the writer at BEGIN
	// so another process cannot invalidate that read snapshot before the write.
	options := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(30000)", "foreign_keys(1)", "journal_mode(WAL)"}}
	dsn := path + "?" + options.Encode()
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		u := url.URL{Scheme: "file", Path: absolute, RawQuery: options.Encode()}
		dsn = u.String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Concurrent first opens can collide while enabling WAL, before SQLite's
	// busy handler can wait. Retry only this idempotent initialization step.
	var initializeErr error
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, initializeErr = db.Exec("PRAGMA busy_timeout=30000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON")
		var sqliteError interface{ Code() int }
		if initializeErr == nil || !errors.As(initializeErr, &sqliteError) || sqliteError.Code()&255 != 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if initializeErr != nil {
		db.Close()
		return nil, fmt.Errorf("initializing database: %w", initializeErr)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
CREATE TABLE IF NOT EXISTS symbol_scopes(symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,repo TEXT NOT NULL,namespace TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS symbol_scopes_namespace ON symbol_scopes(repo,namespace);
CREATE TABLE IF NOT EXISTS symbol_content(symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,source_hash TEXT NOT NULL,end_line INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS symbol_receivers(symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,receiver TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS relation_targets(relation_id TEXT PRIMARY KEY REFERENCES relations(id) ON DELETE CASCADE,from_id TEXT NOT NULL,to_id TEXT,namespace TEXT NOT NULL,hint TEXT NOT NULL,resolution TEXT NOT NULL DEFAULT 'unresolved');
CREATE INDEX IF NOT EXISTS relation_targets_from ON relation_targets(from_id);
CREATE INDEX IF NOT EXISTS relation_targets_to ON relation_targets(to_id);

`
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 {
		// The old derived index had unscoped IDs and potentially stale FTS
		// entries. Rebuild it once rather than carrying corruption forward.
		if _, err := tx.Exec("INSERT INTO symbol_fts(symbol_fts) VALUES('rebuild'); DELETE FROM symbols; DELETE FROM relations; DELETE FROM file_cache; DELETE FROM file_cache_v2; DELETE FROM symbol_embeddings WHERE symbol_id NOT IN (SELECT id FROM symbols); DELETE FROM relations; DELETE FROM symbols; DELETE FROM file_cache_v2; DELETE FROM repo_state; PRAGMA user_version=3"); err != nil {
			return err
		}
		return tx.Commit()
	}
	if version < 3 {
		if _, err := tx.Exec("DELETE FROM symbol_embeddings WHERE symbol_id NOT IN (SELECT id FROM symbols); DELETE FROM relations; DELETE FROM symbols; DELETE FROM file_cache_v2; DELETE FROM repo_state; PRAGMA user_version=3"); err != nil {
			return fmt.Errorf("cleaning obsolete embeddings: %w", err)
		}
	}
	return tx.Commit()
}
