package graph

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// trustStatKeys moves the index clock past the racy window so files the
// test just wrote can be skipped by their stat key.
func trustStatKeys(t *testing.T) {
	t.Helper()
	original := indexClock
	indexClock = func() time.Time { return time.Now().Add(time.Hour) }
	t.Cleanup(func() { indexClock = original })
}

type incrementalRepo struct {
	t    *testing.T
	root string
	db   *sql.DB
}

func newIncrementalRepo(t *testing.T, files map[string]string) *incrementalRepo {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := &incrementalRepo{t: t, root: root, db: db}
	for rel, content := range files {
		r.write(rel, content)
	}
	return r
}

func (r *incrementalRepo) write(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *incrementalRepo) analyze() Report {
	r.t.Helper()
	report, err := AnalyzeRepoReport(context.Background(), r.root, r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	return report
}

func (r *incrementalRepo) names() string {
	r.t.Helper()
	rows, err := r.db.Query("SELECT file||':'||name FROM symbols WHERE repo=? ORDER BY file,name", r.root)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			r.t.Fatal(err)
		}
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

// waitForNewChangeTime polls until a write gets an inode change time later
// than after. Kernels stamp change times from a coarse clock, so two writes
// within one tick can share it; production indexing never trusts a stat key
// that recent (racyWindow), but these tests disable that guard.
func waitForNewChangeTime(t *testing.T, dir string, after int64) {
	t.Helper()
	probe := filepath.Join(dir, ".probe")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := os.WriteFile(probe, nil, 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(probe)
		if err != nil {
			t.Fatal(err)
		}
		if ctime, _, _, ok := fileChangeTime(info); !ok || ctime > after {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("change time did not advance")
		}
		time.Sleep(time.Millisecond)
	}
}

func changeTime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctime, _, _, _ := fileChangeTime(info)
	return ctime
}

func TestIncrementalRefreshSkipsUnchangedFilesByStat(t *testing.T) {
	if _, _, _, ok := fileChangeTime(mustStat(t, ".")); !ok {
		t.Skip("no inode change time on this platform")
	}
	trustStatKeys(t)
	r := newIncrementalRepo(t, map[string]string{
		"a.go":    "package p\nfunc A() { B() }\n",
		"b.go":    "package p\nfunc B() {}\n",
		"c.py":    "def c():\n    pass\n",
		"d/e.ts":  "export function e() {}\n",
		"old.go":  "package p\nfunc Old() {}\n",
		"same.go": "package p\nfunc Same() {}\n",
	})
	if got := r.analyze(); got.Files != 6 || got.Read != 6 || got.Analyzed != 6 {
		t.Fatalf("cold index: %+v", got)
	}
	before := r.names()

	// Nothing changed: nothing is read or hashed.
	if got := r.analyze(); got.Read != 0 || got.Analyzed != 0 || got.Files != 6 {
		t.Fatalf("unchanged refresh read sources: %+v", got)
	}
	if after := r.names(); after != before {
		t.Fatalf("unchanged refresh changed symbols: %s != %s", after, before)
	}

	// Same size and restored modification time: the change time differs.
	path := filepath.Join(r.root, "b.go")
	info := mustStat(t, path)
	waitForNewChangeTime(t, r.root, changeTime(t, path))
	r.write("b.go", "package p\nfunc C() {}\n")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := r.analyze(); got.Read != 1 || got.Analyzed != 1 {
		t.Fatalf("same-size edit with restored mtime: %+v", got)
	}
	if names := r.names(); !strings.Contains(names, "b.go:C") || strings.Contains(names, "b.go:B") {
		t.Fatalf("edit not indexed: %s", names)
	}

	// Touched but identical: hashed once, not analyzed, then skipped again.
	later := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(r.root, "same.go"), later, later); err != nil {
		t.Fatal(err)
	}
	if got := r.analyze(); got.Read != 1 || got.Analyzed != 0 {
		t.Fatalf("touch: %+v", got)
	}
	if got := r.analyze(); got.Read != 0 {
		t.Fatalf("touched file's new stat key was not stored: %+v", got)
	}

	// Deleted and renamed files.
	if err := os.Remove(filepath.Join(r.root, "old.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.root, "d", "e.ts"), filepath.Join(r.root, "d", "renamed.ts")); err != nil {
		t.Fatal(err)
	}
	got := r.analyze()
	if got.Files != 5 || got.Read != 1 || got.Analyzed != 1 {
		t.Fatalf("delete and rename: %+v", got)
	}
	names := r.names()
	if strings.Contains(names, "Old") || strings.Contains(names, "d/e.ts") || !strings.Contains(names, "d/renamed.ts:e") {
		t.Fatalf("deleted or renamed file stale: %s", names)
	}
	var cached int
	if err := r.db.QueryRow("SELECT count(*) FROM file_cache_v2 WHERE repo=? AND path IN ('old.go','d/e.ts')", r.root).Scan(&cached); err != nil || cached != 0 {
		t.Fatalf("removed paths still cached: %d %v", cached, err)
	}

	// An atomic replace with identical size and times has a new inode.
	replacement := filepath.Join(r.root, "c.py.tmp")
	info = mustStat(t, filepath.Join(r.root, "c.py"))
	if err := os.WriteFile(replacement, []byte("def z():\n    pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(r.root, "c.py")); err != nil {
		t.Fatal(err)
	}
	if got := r.analyze(); got.Read != 1 || got.Analyzed != 1 || !strings.Contains(r.names(), "c.py:z") {
		t.Fatalf("atomic replace missed: %+v %s", got, r.names())
	}

	// A go.mod edit changes every cache key, so every source is analyzed.
	r.write("go.mod", "module example.com/m\n")
	if got := r.analyze(); got.Read != 5 || got.Analyzed != 5 {
		t.Fatalf("go.mod edit did not invalidate stat keys: %+v", got)
	}
}

// Files changed within the racy window are hashed again next time, because
// a coarse timestamp could hide a second same-size write.
func TestIncrementalRefreshDistrustsRecentStatKeys(t *testing.T) {
	r := newIncrementalRepo(t, map[string]string{"a.go": "package p\nfunc A() {}\n"})
	r.analyze()
	var stat sql.NullString
	if err := r.db.QueryRow("SELECT stat FROM file_cache_v2 WHERE repo=? AND path='a.go'", r.root).Scan(&stat); err != nil || stat.Valid {
		t.Fatalf("recent file got a trusted stat key: %v %v", stat, err)
	}
	if got := r.analyze(); got.Read != 1 || got.Analyzed != 0 {
		t.Fatalf("recent file not hashed again: %+v", got)
	}
}

// An unchanged file skipped by its stat key still takes part in the Go type
// pass when another file changes.
func TestIncrementalRefreshTypeChecksUnreadSources(t *testing.T) {
	if !goTypesEnabled {
		t.Skip("no Go type pass in this build")
	}
	if _, _, _, ok := fileChangeTime(mustStat(t, ".")); !ok {
		t.Skip("no inode change time on this platform")
	}
	trustStatKeys(t)
	r := newIncrementalRepo(t, map[string]string{
		"go.mod": "module example.com/m\n",
		"a.go":   "package p\nfunc A() { var t T; t.M() }\n",
		"b.go":   "package p\ntype T struct{}\nfunc (T) M() {}\n",
	})
	r.analyze()
	typed := func() int {
		var n int
		if err := r.db.QueryRow("SELECT count(*) FROM relation_targets t JOIN relations rel ON rel.id=t.relation_id WHERE rel.repo=? AND rel.to_symbol='M' AND t.resolution='typed'", r.root).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if typed() != 1 {
		t.Fatal("cold index did not type the call")
	}
	path := filepath.Join(r.root, "a.go")
	waitForNewChangeTime(t, r.root, changeTime(t, path))
	r.write("a.go", "package p\n\n// edited\nfunc A() { var t T; t.M() }\n")
	if got := r.analyze(); got.Read != 1 || got.Analyzed != 1 {
		t.Fatalf("edit: %+v", got)
	}
	if typed() != 1 {
		t.Fatal("type pass lost the unread source b.go")
	}
}

func mustStat(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
