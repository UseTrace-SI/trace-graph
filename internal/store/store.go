package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/ident"
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
  name TEXT NOT NULL, kind TEXT NOT NULL, line INTEGER NOT NULL, signature TEXT, terms TEXT);
CREATE INDEX IF NOT EXISTS symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS symbols_file ON symbols(file);
CREATE INDEX IF NOT EXISTS symbols_repo_file ON symbols(repo,file,line);
CREATE INDEX IF NOT EXISTS symbols_repo_name ON symbols(repo,name);
CREATE TABLE IF NOT EXISTS file_cache (path TEXT PRIMARY KEY, mtime INTEGER NOT NULL, size INTEGER NOT NULL);
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
CREATE INDEX IF NOT EXISTS relations_repo_file ON relations(repo,file);
CREATE TABLE IF NOT EXISTS symbol_scopes(symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,repo TEXT NOT NULL,namespace TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS symbol_scopes_namespace ON symbol_scopes(repo,namespace);
CREATE TABLE IF NOT EXISTS symbol_content(symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,source_hash TEXT NOT NULL,end_line INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS symbol_receivers(symbol_id TEXT PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,receiver TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS relation_targets(relation_id TEXT PRIMARY KEY REFERENCES relations(id) ON DELETE CASCADE,from_id TEXT NOT NULL,to_id TEXT,namespace TEXT NOT NULL,hint TEXT NOT NULL,resolution TEXT NOT NULL DEFAULT 'unresolved');
CREATE INDEX IF NOT EXISTS relation_targets_from ON relation_targets(from_id);
CREATE INDEX IF NOT EXISTS relation_targets_to ON relation_targets(to_id);
CREATE TABLE IF NOT EXISTS repo_resolution(repo TEXT PRIMARY KEY,complete INTEGER NOT NULL,detail TEXT NOT NULL);
`
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	// Schema 4 replaces the search index first, so the clean-ups below
	// delete through triggers that match the index they maintain.
	if version < 4 {
		if err := migrateSearchTerms(tx); err != nil {
			return fmt.Errorf("indexing identifier words: %w", err)
		}
	}
	if version < 1 {
		// The old derived index had unscoped IDs and potentially stale FTS
		// entries. Rebuild it once rather than carrying corruption forward.
		if _, err := tx.Exec("DELETE FROM symbols; DELETE FROM relations; DELETE FROM file_cache; DELETE FROM file_cache_v2; DELETE FROM symbol_embeddings WHERE symbol_id NOT IN (SELECT id FROM symbols); DELETE FROM repo_state"); err != nil {
			return err
		}
	} else if version < 3 {
		if _, err := tx.Exec("DELETE FROM symbol_embeddings WHERE symbol_id NOT IN (SELECT id FROM symbols); DELETE FROM relations; DELETE FROM symbols; DELETE FROM file_cache_v2; DELETE FROM repo_state"); err != nil {
			return fmt.Errorf("cleaning obsolete embeddings: %w", err)
		}
	}
	if version >= 1 && version < 5 {
		// Schema 5 embeds split identifiers and the file path. Derived
		// vectors are recomputed by the next analysis, which embeds every
		// symbol without one.
		if _, err := tx.Exec("DELETE FROM symbol_embeddings"); err != nil {
			return fmt.Errorf("clearing outdated embeddings: %w", err)
		}
	}
	if version < SchemaVersion {
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SchemaVersion is the user_version of a fully migrated index. Read-only
// commands never migrate, so they check it before using newer columns.
const SchemaVersion = 5

// searchSchema indexes each symbol's name, its split identifier words
// (terms) and its signature. Terms are computed in Go when a symbol is
// written, so the triggers only copy them. It is created by the schema 4
// migration, which every new index runs.
const searchSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(name, terms, signature, content='symbols', content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS symbols_ai AFTER INSERT ON symbols BEGIN
  INSERT INTO symbol_fts(rowid,name,terms,signature) VALUES(new.rowid,new.name,new.terms,new.signature);
END;
CREATE TRIGGER IF NOT EXISTS symbols_ad AFTER DELETE ON symbols BEGIN
  INSERT INTO symbol_fts(symbol_fts,rowid,name,terms,signature) VALUES('delete',old.rowid,old.name,old.terms,old.signature);
END;
CREATE TRIGGER IF NOT EXISTS symbols_au AFTER UPDATE ON symbols BEGIN
  INSERT INTO symbol_fts(symbol_fts,rowid,name,terms,signature) VALUES('delete',old.rowid,old.name,old.terms,old.signature);
  INSERT INTO symbol_fts(rowid,name,terms,signature) VALUES(new.rowid,new.name,new.terms,new.signature);
END;
`

// migrateSearchTerms (schema 4) adds split identifier words to existing
// symbols and rebuilds the search index with them, keeping the indexed
// generation usable without a re-analysis.
func migrateSearchTerms(tx *sql.Tx) error {
	var hasTerms int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('symbols') WHERE name='terms'").Scan(&hasTerms); err != nil {
		return err
	}
	// The previous index and its triggers are replaced; terms are filled
	// while no trigger can mirror a half-written row into the index.
	if _, err := tx.Exec("DROP TRIGGER IF EXISTS symbols_ai; DROP TRIGGER IF EXISTS symbols_ad; DROP TRIGGER IF EXISTS symbols_au; DROP TABLE IF EXISTS symbol_fts"); err != nil {
		return err
	}
	if hasTerms == 0 {
		if _, err := tx.Exec("ALTER TABLE symbols ADD COLUMN terms TEXT"); err != nil {
			return err
		}
	}
	rows, err := tx.Query("SELECT rowid,name FROM symbols WHERE terms IS NULL")
	if err != nil {
		return err
	}
	type row struct {
		rowid int64
		name  string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rowid, &r.name); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	update, err := tx.Prepare("UPDATE symbols SET terms=? WHERE rowid=?")
	if err != nil {
		return err
	}
	defer update.Close()
	for _, r := range pending {
		if _, err := update.Exec(ident.Terms(r.name), r.rowid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(searchSchema + "INSERT INTO symbol_fts(symbol_fts) VALUES('rebuild');"); err != nil {
		return err
	}
	return nil
}
