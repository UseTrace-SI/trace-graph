package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/embed"
)

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

// AnalyzeRepoContext indexes a repository and returns the number of symbols
// written by this generation.
func AnalyzeRepoContext(ctx context.Context, repo string, db *sql.DB) (int, error) {
	report, err := AnalyzeRepoReport(ctx, repo, db)
	return report.Symbols, err
}

// FileWarning names a source file the index leaves out and why.
type FileWarning struct {
	Path    string `json:"path"`
	Warning string `json:"warning"`
}

// Report summarizes a committed generation.
type Report struct {
	// Symbols counts the symbols written by this generation.
	Symbols int
	// Skipped lists every source currently left out of the index.
	Skipped []FileWarning
}

// maxReportedWarnings bounds the skipped files listed in a Report.
const maxReportedWarnings = 100

// AnalyzeRepoReport is AnalyzeRepoContext with the files the generation
// skipped. A source whose declarations cannot be given distinct identities
// (two same-named declarations on one line, typical of minified code) is
// skipped with a recorded warning rather than failing the whole index; it is
// analyzed again once its content changes.
func AnalyzeRepoReport(ctx context.Context, repo string, db *sql.DB) (Report, error) {
	n, err := analyzeRepo(ctx, repo, db)
	if err != nil {
		return Report{}, err
	}
	report := Report{Symbols: n, Skipped: []FileWarning{}}
	root, err := canonicalRoot(repo)
	if err != nil {
		return Report{}, err
	}
	report.Skipped, err = FileWarnings(ctx, db, root, maxReportedWarnings)
	return report, err
}

// FileWarnings lists at most limit skipped sources of a repository in path
// order. An index without the warnings table has none.
func FileWarnings(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, repo string, limit int) ([]FileWarning, error) {
	warnings := []FileWarning{}
	rows, err := db.QueryContext(ctx, "SELECT path,warning FROM file_warnings WHERE repo=? ORDER BY path LIMIT ?", repo, limit)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return warnings, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w FileWarning
		if err := rows.Scan(&w.Path, &w.Warning); err != nil {
			return nil, err
		}
		warnings = append(warnings, w)
	}
	return warnings, rows.Err()
}

func canonicalRoot(repo string) (string, error) {
	root, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolving repository: %w", err)
	}
	return root, nil
}

func analyzeRepo(ctx context.Context, repo string, db *sql.DB) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	root, err := canonicalRoot(repo)
	if err != nil {
		return 0, err
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
	defer w.close()
	seen := map[string]bool{}
	n, analyzed := 0, 0
	var totalBytes int64
	// Every Go file, cached or not, takes part in package-wide type checking,
	// except skipped files, whose declarations are not in the index.
	var goSources []goSource
	goBytes, goOverBudget := 0, false
	skipped, err := warnedFiles(ctx, tx, root)
	if err != nil {
		return 0, err
	}
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
		typeChecked := goTypesEnabled && strings.EqualFold(filepath.Ext(rel), ".go")
		if typeChecked {
			if goBytes += len(src); goBytes > goTypeSourceBudget {
				goOverBudget, goSources = true, nil
			}
		}
		addGoSource := func() {
			if typeChecked && !goOverBudget {
				goSources = append(goSources, goSource{rel: rel, src: src})
			}
		}
		// Type-checked resolution follows the platform's build constraints.
		digest := fmt.Sprintf("%s@%s/%s:%s:%x", analyzerVersion, runtime.GOOS, runtime.GOARCH, modDigest, sha256.Sum256(src))
		var old string
		err = tx.QueryRowContext(ctx, "SELECT digest FROM file_cache_v2 WHERE repo=? AND path=?", root, rel).Scan(&old)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if old == digest {
			if !skipped[rel] {
				addGoSource()
			}
			return nil
		}
		if err := removeFile(tx, root, rel); err != nil {
			return err
		}
		batch := w.batch(rel)
		count, err := analyzeFile(root, rel, path, src, batch)
		if err != nil {
			return err
		}
		var collision *collisionError
		if err := w.flush(root, batch); errors.As(err, &collision) {
			// The batch wrote nothing; the file stays out of this generation
			// until its content changes.
			if _, err := tx.Exec("INSERT INTO file_warnings(repo,path,warning) VALUES(?,?,?)", root, rel, collision.Error()); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			n += count
			addGoSource()
		}
		if _, err := tx.Exec("INSERT INTO file_cache_v2(repo,path,digest) VALUES(?,?,?)", root, rel, digest); err != nil {
			return err
		}
		analyzed++
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
	if err := resolveRepository(ctx, tx, root, analyzed > 0 || len(deleted) > 0, goSources, goOverBudget); err != nil {
		return 0, err
	}
	symRows, err := tx.QueryContext(ctx, "SELECT s.id,s.file,s.name,COALESCE(s.signature,'') FROM symbols s LEFT JOIN symbol_embeddings e ON e.symbol_id=s.id WHERE s.repo=? AND e.symbol_id IS NULL", root)
	if err != nil {
		return 0, err
	}
	type sym struct{ id, text string }
	var syms []sym
	for symRows.Next() {
		var id, file, name, sig string
		if err := symRows.Scan(&id, &file, &name, &sig); err != nil {
			symRows.Close()
			return 0, err
		}
		syms = append(syms, sym{id: id, text: embed.SymbolText(file, name, sig)})
	}
	symRows.Close()
	if err := symRows.Err(); err != nil {
		return 0, err
	}
	if len(syms) > 0 {
		insert, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO symbol_embeddings(symbol_id,dim,vector) VALUES(?,?,?)")
		if err != nil {
			return 0, err
		}
		for _, s := range syms {
			if _, err := insert.Exec(s.id, embed.Dim, embed.Encode(embed.Embed(s.text))); err != nil {
				insert.Close()
				return 0, err
			}
		}
		insert.Close()
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

// resolveRepository recomputes every relation target of a generation. With
// no changed or deleted file, a complete previous pass is still exact, so an
// unchanged refresh skips the work.
func resolveRepository(ctx context.Context, tx *sql.Tx, root string, changed bool, goSources []goSource, goOverBudget bool) error {
	if !changed {
		var complete int
		err := tx.QueryRowContext(ctx, "SELECT complete FROM repo_resolution WHERE repo=?", root).Scan(&complete)
		if err == nil && complete == 1 {
			return nil
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	// Edges derived by the Go type pass depend on other files, so they are
	// always rebuilt. Lexical implements edges belong to their file.
	if _, err := tx.ExecContext(ctx, "DELETE FROM relations WHERE repo=? AND kind IN ('implements','dispatch') AND id IN (SELECT relation_id FROM relation_targets WHERE hint='implements' OR substr(hint,1,9)='dispatch:')", root); err != nil {
		return err
	}
	if err := resolveCalls(ctx, tx, root); err != nil {
		return err
	}
	if err := resolveLexical(ctx, tx, root); err != nil {
		return fmt.Errorf("resolving imports and lexical calls: %w", err)
	}
	stats, err := resolveGoTypes(ctx, tx, root, moduleName(root), goSources, goOverBudget)
	if err != nil {
		return fmt.Errorf("resolving Go types: %w", err)
	}
	detail, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	complete := 1
	if stats.Status == "partial" {
		complete = 0
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO repo_resolution(repo,complete,detail) VALUES(?,?,?) ON CONFLICT(repo) DO UPDATE SET complete=excluded.complete,detail=excluded.detail", root, complete, string(detail))
	return err
}

// warnedFiles returns the skipped sources of the previous generation.
func warnedFiles(ctx context.Context, tx *sql.Tx, repo string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT path FROM file_warnings WHERE repo=?", repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths[path] = true
	}
	return paths, rows.Err()
}

func removeFile(tx *sql.Tx, repo, path string) error {
	for _, table := range []string{"symbols", "relations", "file_cache_v2", "file_warnings"} {
		column := "file"
		if table == "file_cache_v2" || table == "file_warnings" {
			column = "path"
		}
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE repo=? AND "+column+"=?", repo, path); err != nil {
			return err
		}
	}
	return nil
}
