package graph

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func revisionTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func revisionTestWrite(t *testing.T, root, file, content string) {
	t.Helper()
	name := filepath.Join(root, file)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func revisionTestCommit(t *testing.T, root string) string {
	t.Helper()
	revisionTestGit(t, root, "add", "-A")
	revisionTestGit(t, root, "commit", "-qm", "fixture")
	return revisionTestGit(t, root, "rev-parse", "HEAD")
}
func revisionTestRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	revisionTestGit(t, root, "init", "-q")
	return root
}

func TestRevisionIndexesUseCommittedSourcesModuleAndUnionOfExclusions(t *testing.T) {
	ctx := context.Background()
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "go.mod", "module example.test/old\n")
	revisionTestWrite(t, root, "a/a.go", "package a\nimport b \"example.test/old/b\"\nfunc Caller(){b.Target()}\n")
	revisionTestWrite(t, root, "b/b.go", "package b\nfunc Target(){}\n")
	for _, file := range []string{"base_private.go", "head_private.go", "current_private.go", ".env/private.go", ".AWS/private.go", ".ENV/private.go"} {
		revisionTestWrite(t, root, file, "package p\nfunc MustNeverAppear(){}\n")
	}
	revisionTestWrite(t, root, ".traceignore", "base_private.go\n")
	if err := os.Symlink("b/b.go", filepath.Join(root, "linked.go")); err != nil {
		t.Fatal(err)
	}
	base := revisionTestCommit(t, root)
	revisionTestWrite(t, root, "go.mod", "module example.test/new\n")
	revisionTestWrite(t, root, "a/a.go", "package a\nimport b \"example.test/new/b\"\nfunc Caller(){b.Target()}\n")
	revisionTestWrite(t, root, ".traceignore", "head_private.go\n")
	head := revisionTestCommit(t, root)
	revisionTestWrite(t, root, ".traceignore", "current_private.go\n")
	revisionTestWrite(t, root, "go.mod", "module irrelevant.example/dirty\n")
	revisionTestWrite(t, root, "a/a.go", "package a\nfunc UncommittedMustNotAppear(){}\n")
	pair, err := PrepareRevisionPair(ctx, root, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if pair.BaseCommit != base || pair.HeadCommit != head || len(pair.PolicyHash) != 64 {
		t.Fatalf("wrong identities: %+v", pair)
	}
	for _, side := range []string{"base", "head"} {
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		stats, err := pair.Analyze(ctx, side, db)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if stats.Files != 2 || stats.NonRegular != 1 || stats.Excluded < 4 {
			t.Fatalf("wrong source selection: %+v", stats)
		}
		var symbols, resolved int
		if err := db.QueryRow("SELECT count(*) FROM symbols").Scan(&symbols); err != nil || symbols != 2 {
			t.Fatalf("unexpected symbols: %d %v", symbols, err)
		}
		if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE to_id IS NOT NULL").Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		want := 1
		if strings.HasPrefix(Capabilities().Analyzer, "treesitter") {
			want = 0
		}
		if resolved != want {
			t.Fatalf("working module leaked into %s: %d resolved", side, resolved)
		}
		db.Close()
	}
	// A third policy change is bound in the report's policy identity.
	revisionTestWrite(t, root, ".traceignore", "current_private.go\nb/\n")
	changed, err := PrepareRevisionPair(ctx, root, base, head)
	if err != nil || changed.PolicyHash == pair.PolicyHash {
		t.Fatal("policy was not fingerprinted", err)
	}
}

func TestRevisionComparisonRejectsCollidingDeclarationIdentities(t *testing.T) {
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "p.go", "package p\ntype T struct{}\ntype U struct{}\nfunc(T)Same(){};func(U)Same(){}\n")
	commit := revisionTestCommit(t, root)
	pair, err := PrepareRevisionPair(context.Background(), root, commit, commit)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := pair.Analyze(context.Background(), "base", db); err == nil || !strings.Contains(err.Error(), "colliding declaration identities") {
		t.Fatal("ambiguous identities were overwritten", err)
	}
}

func TestRevisionComparisonSupportsSHA256CommitIdentities(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	revisionTestGit(t, root, "init", "--object-format=sha256", "-q")
	revisionTestWrite(t, root, "p.go", "package p\nfunc SHA256(){}\n")
	commit := revisionTestCommit(t, root)
	pair, err := PrepareRevisionPair(context.Background(), root, commit, commit)
	if err != nil || len(pair.HeadCommit) != 64 {
		t.Fatal("SHA-256 identity rejected", err)
	}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := pair.Analyze(context.Background(), "head", db); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionAnalysisRefusesPersistentIndexesAndRollsBackFailedMemoryGeneration(t *testing.T) {
	ctx := context.Background()
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "p.go", "package p\nfunc Keep(){}\n")
	commit := revisionTestCommit(t, root)
	pair, err := PrepareRevisionPair(ctx, root, commit, commit)
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	if _, err := AnalyzeRepo(root, persistent); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.Analyze(ctx, "base", persistent); err == nil {
		t.Fatal("accepted a persistent index")
	}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := pair.Analyze(ctx, "base", db); err != nil {
		t.Fatal(err)
	}
	for index := range pair.head.entries {
		if pair.head.entries[index].file == "p.go" {
			pair.head.entries[index].size = 8<<20 + 1
		}
	}
	if _, err := pair.Analyze(ctx, "head", db); err == nil {
		t.Fatal("oversized source accepted")
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name='Keep'").Scan(&n); err != nil || n != 1 {
		t.Fatal("failed generation damaged prior memory index", err)
	}
}

func TestRevisionComparisonRejectsInvalidRefsSubdirectoriesAndCancellation(t *testing.T) {
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "p.go", "package p\nfunc Keep(){}\n")
	commit := revisionTestCommit(t, root)
	for _, ref := range []string{"", "--help", "HEAD\nother", strings.Repeat("x", 257), "HEAD:p.go"} {
		if _, err := PrepareRevisionPair(context.Background(), root, ref, commit); err == nil {
			t.Fatalf("accepted reference %q", ref)
		}
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRevisionPair(context.Background(), sub, commit, commit); err == nil {
		t.Fatal("accepted subdirectory as repository root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareRevisionPair(ctx, root, commit, commit); err == nil {
		t.Fatal("cancelled comparison succeeded")
	}
}

func TestRevisionComparisonIgnoresAmbientGitRedirectionAndReplacementObjects(t *testing.T) {
	ctx := context.Background()
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "p.go", "package p\nfunc Original(){}\n")
	base := revisionTestCommit(t, root)
	revisionTestWrite(t, root, "p.go", "package p\nfunc Replacement(){}\n")
	head := revisionTestCommit(t, root)
	revisionTestGit(t, root, "replace", base, head)
	foreign := revisionTestRepo(t)
	revisionTestWrite(t, foreign, "foreign.go", "package p\nfunc WrongRepository(){}\n")
	revisionTestCommit(t, foreign)
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	pair, err := PrepareRevisionPair(ctx, root, base, base)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := pair.Analyze(ctx, "base", db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name='Original'").Scan(&n); err != nil || n != 1 {
		t.Fatal("ambient Git context or replacement changed source identity", err)
	}
}

func TestRevisionMissingPromisorObjectNeverRunsFetchHelper(t *testing.T) {
	ctx := context.Background()
	root := revisionTestRepo(t)
	revisionTestWrite(t, root, "p.go", "package p\nfunc LocalOnly(){}\n")
	commit := revisionTestCommit(t, root)
	blob := revisionTestGit(t, root, "rev-parse", "HEAD:p.go")
	marker := filepath.Join(t.TempDir(), "fetch-was-executed")
	helper := filepath.Join(t.TempDir(), "fetch-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf attempted > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	revisionTestGit(t, root, "config", "core.repositoryformatversion", "1")
	revisionTestGit(t, root, "config", "extensions.partialClone", "origin")
	revisionTestGit(t, root, "config", "remote.origin.promisor", "true")
	revisionTestGit(t, root, "config", "remote.origin.url", "ext::"+helper)
	revisionTestGit(t, root, "config", "protocol.ext.allow", "always")
	if err := os.Remove(filepath.Join(root, ".git/objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	pair, err := PrepareRevisionPair(ctx, root, commit, commit)
	if err == nil {
		var db *sql.DB
		db, err = store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		_, err = pair.Analyze(ctx, "base", db)
		db.Close()
	}
	if err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("comparison executed a remote helper", err)
	}
}
