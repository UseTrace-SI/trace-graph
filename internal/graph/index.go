package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/embed"
)

// indexWriter preserves the first database error, including writes made from
// parser callbacks. The surrounding transaction is never committed on failure.
type indexWriter struct {
	tx              *sql.Tx
	err             error
	namespace       string
	moduleOverride  *string
	maxRecords      int
	records         int
	symbolIDs       map[string]bool
}

// write is the single counted write path. Symbol metadata rows must share the
// observation cap with symbols and relations, otherwise the stated row budget
// is exceeded roughly threefold by scopes, content and targets alone.
func (w *indexWriter) write(query string, args ...any) (sql.Result, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.maxRecords > 0 && w.records >= w.maxRecords {
		w.err = fmt.Errorf("analysis exceeds %d observation rows", w.maxRecords)
		return nil, w.err
	}
	w.records++
	r, err := w.tx.Exec(query, args...)
	w.err = err
	return r, err
}

func (w *indexWriter) Exec(query string, args ...any) (sql.Result, error) {
	if w.err != nil {
		return nil, w.err
	}
	// Every caller supplies (id, repo, ...). Assert the shape explicitly so a
	// future query with a different argument order fails with a useful error
	// instead of panicking or silently formatting a malformed scoped ID.
	if len(args) < 2 {
		w.err = fmt.Errorf("indexWriter.Exec: expected at least (id, repo) string arguments, got %d", len(args))
		return nil, w.err
	}
	id, ok := args[0].(string)
	if !ok {
		w.err = fmt.Errorf("indexWriter.Exec: argument 0 (id) must be a string, got %T", args[0])
		return nil, w.err
	}
	repo, ok := args[1].(string)
	if !ok {
		w.err = fmt.Errorf("indexWriter.Exec: argument 1 (repo) must be a string, got %T", args[1])
		return nil, w.err
	}
	// Declaration identities are derived from file, name and line. Two
	// same-name declarations on one line would silently overwrite each other
	// (losing signature and receiver data and producing wrong edges), so reject
	// collisions in every mode, not just revision comparisons.
	if strings.Contains(query, "INTO symbols") {
		if w.symbolIDs == nil {
			w.symbolIDs = map[string]bool{}
		}
		if w.symbolIDs[id] {
			w.err = fmt.Errorf("repository contains colliding declaration identities; separate same-name declarations onto different lines")
			return nil, w.err
		}
		w.symbolIDs[id] = true
	}
	// Symbol and relation IDs must be unique across repositories in one DB.
	args[0] = scopedID(repo, id)
	query = strings.Replace(query, "INSERT OR REPLACE", "INSERT", 1)
	if strings.Contains(query, "INTO symbols") {
		query += " ON CONFLICT(id) DO UPDATE SET signature=excluded.signature,kind=excluded.kind"
	} else {
		query += " ON CONFLICT(id) DO NOTHING"
	}
	r, err := w.write(query, args...)
	if err != nil {
		return r, err
	}
	if strings.Contains(query, "INTO symbols") {
		_, w.err = w.write("INSERT INTO symbol_scopes(symbol_id,repo,namespace) VALUES(?,?,?) ON CONFLICT(symbol_id) DO UPDATE SET namespace=excluded.namespace", args[0], args[1], w.namespace)
	}
	return r, w.err
}

func (w *indexWriter) content(repo, id string, src []byte, endLine int) {
	if w.err != nil {
		return
	}
	_, _ = w.write("INSERT INTO symbol_content(symbol_id,source_hash,end_line) VALUES(?,?,?) ON CONFLICT(symbol_id) DO UPDATE SET source_hash=excluded.source_hash,end_line=excluded.end_line", scopedID(repo, id), fmt.Sprintf("%x", sha256.Sum256(src)), endLine)
}

func filepathExt(path string) string { return filepath.Ext(path) }

// isIndexBusy recognizes SQLite writer contention as well as a caller deadline
// spent waiting for the immediate transaction at the start of an analysis.
func isIndexBusy(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var sqliteError interface{ Code() int }
	return errors.As(err, &sqliteError) && sqliteError.Code()&255 == 5 // SQLITE_BUSY
}

func indexBusyError(err error) error {
	return fmt.Errorf("index in progress in another process (or analysis deadline exceeded); retry after it completes: %w", err)
}

// go.mod is configuration, not a source, so it is not walked as a file. Its
// module path still changes how imports resolve. Fold its content into every
// file's cache key so a module-only edit forces a fresh analysis.
func goModDigest(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", fmt.Errorf("go.mod exceeds 1 MiB")
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// AnalyzeRepo commits a complete repository generation atomically. Content
// hashes catch edits even when size and filesystem timestamps are unchanged.
func AnalyzeRepo(repo string, db *sql.DB) (int, error) {
	return AnalyzeRepoContext(context.Background(), repo, db)
}

func AnalyzeRepoContext(ctx context.Context, repo string, db *sql.DB) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	root, err := filepath.Abs(repo)
	if err != nil {
		return 0, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return 0, fmt.Errorf("resolving repository: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("repository is not a directory: %s", root)
	}
	modDigest, err := goModDigest(root)
	if err != nil {
		return 0, fmt.Errorf("reading go.mod: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		if isIndexBusy(err) {
			return 0, indexBusyError(err)
		}
		return 0, err
	}
	defer tx.Rollback()
	w := &indexWriter{tx: tx}
	seen := map[string]bool{}
	n := 0
	var totalBytes int64
	err = walkSourcesContext(ctx, root, func(rel string, f *os.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(root, rel)
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		if len(seen) > 100000 {
			return fmt.Errorf("repository exceeds 100000 source files")
		}
		src, err := io.ReadAll(io.LimitReader(f, 8*1024*1024+1))
		if err != nil {
			return err
		}
		if len(src) > 8*1024*1024 {
			return fmt.Errorf("source exceeds 8 MiB: %s", rel)
		}
		totalBytes += int64(len(src))
		if totalBytes > 512*1024*1024 {
			return fmt.Errorf("repository sources exceed 512 MiB")
		}
		digest := fmt.Sprintf("%s:%s:%x", analyzerVersion, modDigest, sha256.Sum256(src))
		var old string
		err = tx.QueryRowContext(ctx, "SELECT digest FROM file_cache_v2 WHERE repo=? AND path=?", root, rel).Scan(&old)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if old == digest {
			return nil
		}
		if err := removeFile(tx, root, rel); err != nil {
			return err
		}
		count, err := analyzeFile(root, rel, path, src, w)
		if err != nil {
			return err
		}
		if w.err != nil {
			return w.err
		}
		if _, err := tx.Exec("INSERT INTO file_cache_v2(repo,path,digest) VALUES(?,?,?)", root, rel, digest); err != nil {
			return err
		}
		n += count
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("indexing repository: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT path FROM file_cache_v2 WHERE repo=?", root)
	if err != nil {
		return 0, err
	}
	var deleted []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[path] {
			deleted = append(deleted, path)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, path := range deleted {
		if err := removeFile(tx, root, path); err != nil {
			return 0, err
		}
	}
	if err := resolveCalls(ctx, tx, root); err != nil {
		return 0, err
	}
	symRows, err := tx.QueryContext(ctx, "SELECT s.id,s.name,COALESCE(s.signature,'') FROM symbols s LEFT JOIN symbol_embeddings e ON e.symbol_id=s.id WHERE s.repo=? AND e.symbol_id IS NULL", root)
	if err != nil {
		return 0, err
	}
	type sym struct{ id, text string }
	var syms []sym
	for symRows.Next() {
		var id, name, sig string
		if err := symRows.Scan(&id, &name, &sig); err != nil {
			symRows.Close()
			return 0, err
		}
		syms = append(syms, sym{id: id, text: name + " " + sig})
	}
	symRows.Close()
	if err := symRows.Err(); err != nil {
		return 0, err
	}
	for _, s := range syms {
		if _, err := tx.Exec("INSERT OR REPLACE INTO symbol_embeddings(symbol_id,dim,vector) VALUES(?,?,?)",
			s.id, embed.Dim, embed.Encode(embed.Embed(s.text))); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec("INSERT INTO repo_state(repo,indexed_at) VALUES(?,?) ON CONFLICT(repo) DO UPDATE SET indexed_at=excluded.indexed_at", root, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		if isIndexBusy(err) {
			return 0, indexBusyError(err)
		}
		return 0, err
	}
	return n, nil
}

func removeFile(tx *sql.Tx, repo, path string) error {
	for _, table := range []string{"symbols", "relations", "file_cache_v2"} {
		column := "file"
		if table == "file_cache_v2" {
			column = "path"
		}
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE repo=? AND "+column+"=?", repo, path); err != nil {
			return err
		}
	}
	return nil
}
