package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func isolationIndex(t *testing.T, root string) func(name string) int {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	return func(name string) int {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=? AND name=?", root, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// Git exports GIT_DIR, GIT_WORK_TREE and GIT_INDEX_FILE to hooks and their
// children. An indexer inheriting them used to list another repository's
// index: this repository's own sources vanished from a "successful"
// generation, and a file ignored here was indexed because the foreign index
// tracked its name.
func TestRegressionIndexIgnoresAmbientGitRedirection(t *testing.T) {
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, ".gitignore", "ignored.go\n")
	revisionTestWrite(t, root, "tracked.go", "package p\nfunc Tracked(){}\n")
	revisionTestCommit(t, root)
	revisionTestWrite(t, root, "untracked.go", "package p\nfunc Untracked(){}\n")
	revisionTestWrite(t, root, "ignored.go", "package p\nfunc IgnoredHere(){}\n")
	foreign := revisionTestRepo(t)
	revisionTestWrite(t, foreign, "ignored.go", "package p\nfunc ForeignCopy(){}\n")
	revisionTestCommit(t, foreign)

	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(foreign, ".git", "index"))
	count := isolationIndex(t, root)
	for name, want := range map[string]int{"Tracked": 1, "Untracked": 1, "IgnoredHere": 0, "ForeignCopy": 0} {
		if got := count(name); got != want {
			t.Errorf("%s: %d symbols, want %d", name, got, want)
		}
	}
}

// Every refresh used to execute a repository-configured core.fsmonitor
// command through `git ls-files`.
func TestRegressionIndexNeverRunsConfiguredFSMonitor(t *testing.T) {
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "tracked.go", "package p\nfunc Tracked(){}\n")
	revisionTestCommit(t, root)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	revisionTestGit(t, root, "config", "core.fsmonitor", hook)
	if count := isolationIndex(t, root); count("Tracked") != 1 {
		t.Fatal("tracked source missing")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("indexing ran a configured fsmonitor command: %v", err)
	}
}
