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
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
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
	// Files counts the sources of the checkout; Read counts those read and
	// hashed because their stat key did not prove them unchanged, and
	// Analyzed those whose content changed.
	Files, Read, Analyzed int
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
	report, err := analyzeRepo(ctx, repo, db)
	if err != nil {
		return Report{}, err
	}
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

// indexClock is the time stat keys are judged against; tests move it.
var indexClock = time.Now

func analyzeRepo(ctx context.Context, repo string, db *sql.DB) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	root, err := canonicalRoot(repo)
	if err != nil {
		return Report{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return Report{}, err
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("repository is not a directory: %s", root)
	}
	start := indexClock()
	modDigest, err := goModDigest(root)
	if err != nil {
		return Report{}, fmt.Errorf("reading go.mod: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		if isIndexBusy(err) {
			return Report{}, indexBusyError(err)
		}
		return Report{}, err
	}
	defer tx.Rollback()
	w := &indexWriter{tx: tx}
	defer w.close()
	cache, err := loadFileCache(ctx, tx, root)
	if err != nil {
		return Report{}, err
	}
	// Type-checked resolution follows the platform's build constraints.
	digestPrefix := fmt.Sprintf("%s@%s/%s:%s:", analyzerVersion, runtime.GOOS, runtime.GOARCH, modDigest)
	seen := map[string]bool{}
	n, read, analyzed := 0, 0, 0
	var totalBytes int64
	// Every Go file, cached or not, takes part in package-wide type checking,
	// except skipped files, whose declarations are not in the index. Sources
	// skipped by their stat key are read only if the pass runs.
	var goSources []goSource
	goBytes, goOverBudget := 0, false
	// Directories holding type-checked sources; see goDirectoryDigest.
	goDirs := map[string]bool{}
	// Sources are hashed and parsed by a bounded worker pool. Their batches
	// are stored here, by this goroutine alone, in walk order, so a
	// generation is identical to a sequential one.
	pipeline := newIndexPipeline()
	apply := func(job *indexJob) error {
		addGoSource := func() {
			if job.typeChecked && !goOverBudget {
				goSources = append(goSources, goSource{rel: job.rel, src: job.src})
			}
		}
		if job.unchanged {
			if !job.cached.skipped {
				addGoSource() // read later, only if the type pass runs
			}
			return nil
		}
		if job.err != nil {
			return job.err
		}
		if job.identical {
			// Touched but identical: remember the new stat key.
			if !job.cached.skipped {
				addGoSource()
			}
			if job.cached.stat == job.key {
				return nil
			}
			_, err := tx.Exec("UPDATE file_cache_v2 SET stat=? WHERE repo=? AND path=?", nullable(job.key), root, job.rel)
			return err
		}
		if err := removeFile(tx, root, job.rel); err != nil {
			return err
		}
		var collision *collisionError
		if err := w.flush(root, job.batch); errors.As(err, &collision) {
			// The batch wrote nothing; the file stays out of this generation
			// until its content changes.
			if _, err := tx.Exec("INSERT INTO file_warnings(repo,path,warning) VALUES(?,?,?)", root, job.rel, collision.Error()); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			n += job.count
			addGoSource()
		}
		if _, err := tx.Exec("INSERT INTO file_cache_v2(repo,path,digest,stat) VALUES(?,?,?,?)", root, job.rel, job.digest, nullable(job.key)); err != nil {
			return err
		}
		analyzed++
		return nil
	}
	err = walkSourcesContext(ctx, root, func(rel string, f *os.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		job := &indexJob{path: filepath.Join(root, rel), rel: filepath.ToSlash(rel)}
		rel = job.rel
		seen[rel] = true
		if len(seen) > 100000 {
			return fmt.Errorf("repository exceeds 100000 source files")
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		job.key = statKey(info, start)
		job.cached, job.known = cache[rel]
		job.unchanged = job.known && job.key != "" && job.cached.stat == job.key && strings.HasPrefix(job.cached.digest, digestPrefix)
		size := info.Size()
		if !job.unchanged {
			job.src, err = io.ReadAll(io.LimitReader(f, 8*1024*1024+1))
			if err != nil {
				return err
			}
			size = int64(len(job.src))
			read++
		}
		if size > 8*1024*1024 {
			return fmt.Errorf("source exceeds 8 MiB: %s", rel)
		}
		totalBytes += size
		if totalBytes > 512*1024*1024 {
			return fmt.Errorf("repository sources exceed 512 MiB")
		}
		job.typeChecked = goTypesEnabled && strings.EqualFold(filepath.Ext(rel), ".go")
		if job.typeChecked {
			goDirs[pathpkg.Dir(rel)] = true
			if goBytes += int(size); goBytes > goTypeSourceBudget {
				goOverBudget, goSources = true, nil
			}
		}
		if !job.unchanged {
			pipeline.start(job, func() { job.analyze(root, w, digestPrefix) })
		}
		return pipeline.push(job, apply)
	})
	if err == nil {
		err = pipeline.drain(apply)
	}
	if err != nil {
		return Report{}, fmt.Errorf("indexing repository: %w", err)
	}
	var deleted []string
	for path := range cache {
		if !seen[path] {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(deleted)
	for _, path := range deleted {
		if err := removeFile(tx, root, path); err != nil {
			return Report{}, err
		}
	}
	dirDigest, err := goDirectoryDigest(root, goDirs)
	if err != nil {
		return Report{}, err
	}
	resolve, err := resolutionNeeded(ctx, tx, root, analyzed > 0 || len(deleted) > 0, dirDigest)
	if err != nil {
		return Report{}, err
	}
	if resolve {
		if goSources, err = readUnchangedGoSources(root, goSources); err != nil {
			return Report{}, fmt.Errorf("reading Go sources: %w", err)
		}
		if err := resolveRepository(ctx, tx, root, goSources, goOverBudget); err != nil {
			return Report{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO repo_go_dirs(repo,digest) VALUES(?,?) ON CONFLICT(repo) DO UPDATE SET digest=excluded.digest", root, dirDigest); err != nil {
			return Report{}, err
		}
	}
	symRows, err := tx.QueryContext(ctx, "SELECT s.id,s.file,s.name,COALESCE(s.signature,'') FROM symbols s LEFT JOIN symbol_embeddings e ON e.symbol_id=s.id WHERE s.repo=? AND e.symbol_id IS NULL", root)
	if err != nil {
		return Report{}, err
	}
	type sym struct{ id, text string }
	var syms []sym
	for symRows.Next() {
		var id, file, name, sig string
		if err := symRows.Scan(&id, &file, &name, &sig); err != nil {
			symRows.Close()
			return Report{}, err
		}
		syms = append(syms, sym{id: id, text: embed.SymbolText(file, name, sig)})
	}
	symRows.Close()
	if err := symRows.Err(); err != nil {
		return Report{}, err
	}
	if len(syms) > 0 {
		insert, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO symbol_embeddings(symbol_id,dim,vector) VALUES(?,?,?)")
		if err != nil {
			return Report{}, err
		}
		for _, s := range syms {
			if _, err := insert.Exec(s.id, embed.Dim, embed.Encode(embed.Embed(s.text))); err != nil {
				insert.Close()
				return Report{}, err
			}
		}
		insert.Close()
	}
	if _, err := tx.Exec("INSERT INTO repo_state(repo,indexed_at) VALUES(?,?) ON CONFLICT(repo) DO UPDATE SET indexed_at=excluded.indexed_at", root, time.Now().UnixMilli()); err != nil {
		return Report{}, err
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		if isIndexBusy(err) {
			return Report{}, indexBusyError(err)
		}
		return Report{}, err
	}
	return Report{Symbols: n, Files: len(seen), Read: read, Analyzed: analyzed}, nil
}

// resolutionNeeded reports whether relation targets must be recomputed.
// With no changed or deleted file, a complete previous pass is still exact,
// so an unchanged refresh skips the work, unless the Go files beside the
// indexed ones changed (dirDigest).
func resolutionNeeded(ctx context.Context, tx *sql.Tx, root string, changed bool, dirDigest string) (bool, error) {
	if changed {
		return true, nil
	}
	var complete int
	err := tx.QueryRowContext(ctx, "SELECT complete FROM repo_resolution WHERE repo=?", root).Scan(&complete)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil || complete != 1 {
		return true, err
	}
	var previous string
	err = tx.QueryRowContext(ctx, "SELECT digest FROM repo_go_dirs WHERE repo=?", root).Scan(&previous)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return previous != dirDigest, err
}

// goDirectoryDigest fingerprints the Go file entries (name and type) of the
// directories holding type-checked sources. The type pass treats a package
// whose directory holds a Go file it did not see (ignored, excluded,
// symlinked) as incomplete, so adding or removing such a file changes typed
// edges although no indexed source changed. Only entry names are read.
func goDirectoryDigest(root string, dirs map[string]bool) (string, error) {
	if !goTypesEnabled || len(dirs) == 0 {
		return "", nil
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer anchor.Close()
	names := make([]string, 0, len(dirs))
	for dir := range dirs {
		names = append(names, dir)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, dir := range names {
		fmt.Fprintf(h, "%s\x00", dir)
		handle, err := anchor.Open(dir)
		if err != nil {
			fmt.Fprintf(h, "unreadable\n")
			continue
		}
		entries, err := handle.ReadDir(-1)
		handle.Close()
		if err != nil {
			fmt.Fprintf(h, "unreadable\n")
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".go") {
				fmt.Fprintf(h, "%s\x00%d\x00", entry.Name(), entry.Type())
			}
		}
		fmt.Fprintf(h, "\n")
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// resolveRepository recomputes every relation target of a generation.
func resolveRepository(ctx context.Context, tx *sql.Tx, root string, goSources []goSource, goOverBudget bool) error {
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

// cachedFile is the previous generation's record of a source.
type cachedFile struct {
	digest, stat string
	// skipped files have a recorded warning and no declarations.
	skipped bool
}

func loadFileCache(ctx context.Context, tx *sql.Tx, repo string) (map[string]cachedFile, error) {
	rows, err := tx.QueryContext(ctx, "SELECT c.path,c.digest,COALESCE(c.stat,''),w.path IS NOT NULL FROM file_cache_v2 c LEFT JOIN file_warnings w ON w.repo=c.repo AND w.path=c.path WHERE c.repo=?", repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cache := map[string]cachedFile{}
	for rows.Next() {
		var path string
		var file cachedFile
		if err := rows.Scan(&path, &file.digest, &file.stat, &file.skipped); err != nil {
			return nil, err
		}
		cache[path] = file
	}
	return cache, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// readUnchangedGoSources reads the Go sources the walk skipped by their stat
// key (those with a nil src), anchored to the repository root like the walk.
// A source replaced or removed since then is read as it is now (or left
// out); its stored stat key no longer matches, so the next refresh analyzes
// it again.
func readUnchangedGoSources(root string, sources []goSource) ([]goSource, error) {
	var anchor *os.Root
	defer func() {
		if anchor != nil {
			anchor.Close()
		}
	}()
	kept := sources[:0]
	for _, source := range sources {
		if source.src == nil {
			if anchor == nil {
				var err error
				if anchor, err = os.OpenRoot(root); err != nil {
					return nil, err
				}
			}
			file, err := openRegularSource(anchor, filepath.FromSlash(source.rel))
			if err != nil {
				return nil, err
			}
			if file == nil {
				continue
			}
			source.src, err = io.ReadAll(io.LimitReader(file, 8*1024*1024))
			file.Close()
			if err != nil {
				return nil, err
			}
			if source.src == nil {
				source.src = []byte{}
			}
		}
		kept = append(kept, source)
	}
	return kept, nil
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
