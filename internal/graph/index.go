package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/embed"
)

// indexWriter preserves the first database error, including writes made from
// parser callbacks. The surrounding transaction is never committed on failure.
type indexWriter struct {
	tx  *sql.Tx
	err error
}

func (w *indexWriter) Exec(query string, args ...any) (sql.Result, error) {
	if w.err != nil {
		return nil, w.err
	}
	// Symbol and relation IDs must be unique across repositories in one DB.
	args[0] = fmt.Sprintf("%x:%s", sha256.Sum256([]byte(args[1].(string))), args[0])
	query = strings.Replace(query, "INSERT OR REPLACE", "INSERT", 1)
	if strings.Contains(query, "INTO symbols") {
		query += " ON CONFLICT(id) DO UPDATE SET signature=excluded.signature,kind=excluded.kind"
	} else {
		query += " ON CONFLICT(id) DO NOTHING"
	}
	r, err := w.tx.Exec(query, args...)
	w.err = err
	return r, err
}

func filepathExt(path string) string { return filepath.Ext(path) }

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
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	w := &indexWriter{tx: tx}
	seen := map[string]bool{}
	n := 0
	var totalBytes int64
	err = walkSourcesContext(ctx, root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() || !supportedFile(strings.ToLower(filepath.Ext(path))) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		if len(seen) > 100000 {
			return fmt.Errorf("repository exceeds 100000 source files")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		src, err := io.ReadAll(io.LimitReader(f, 8*1024*1024+1))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(src) > 8*1024*1024 {
			return fmt.Errorf("source exceeds 8 MiB: %s", rel)
		}
		totalBytes += int64(len(src))
		if totalBytes > 512*1024*1024 {
			return fmt.Errorf("repository sources exceed 512 MiB")
		}
		digest := fmt.Sprintf("%s:%x", analyzerVersion, sha256.Sum256(src))
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
