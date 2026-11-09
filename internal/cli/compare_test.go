package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
)

func compareTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0"}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
func TestCommitComparisonPreservesWorkingTreeIndexRefsAndExistingGraph(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	write := func(file, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, file), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", "fixture")
		return compareTestGit(t, root, "rev-parse", "HEAD")
	}
	write(".gitattributes", "*.go filter=must-not-run\n")
	write("p.go", "package p\nfunc Stable(){}\nfunc Body()int{return 1}\nfunc Signature(x int){}\nfunc Removed(){}\nfunc Move(){}\n")
	base := commit()
	secret := "ghp_abcdefghijklmnopqrstuvwxyz"
	write("p.go", "package p\nfunc Stable(){}\nfunc Body()int{return 2}\nfunc Signature(x string){}\nfunc Move(){}\nfunc Added(x [len(\""+secret+"\")]byte){}\n")
	head := commit()
	write("p.go", "package p\nfunc DirtyMustNotAppear(){}\n")
	write("untracked.go", "package p\nfunc UntrackedMustNotAppear(){}\n")
	index := filepath.Join(t.TempDir(), "existing-index.db")
	original := []byte("preserve existing index bytes")
	if err := os.WriteFile(index, original, 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "filter-ran")
	helper := filepath.Join(t.TempDir(), "filter")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf ran > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "config", "filter.must-not-run.clean", helper)
	compareTestGit(t, root, "config", "filter.must-not-run.smudge", helper)
	indexBefore, err := os.ReadFile(filepath.Join(root, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	refsBefore := compareTestGit(t, root, "show-ref")
	var output bytes.Buffer
	command := newRoot()
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--repo", root, "--db", index, "compare", "--base", base, "--head", "HEAD"})
	if err := command.Execute(); err != nil {
		t.Fatalf("%s %v", &output, err)
	}
	var report struct {
		BaseCommit string           `json:"base_commit"`
		HeadCommit string           `json:"head_commit"`
		Changes    []revisionChange `json:"changes"`
		Signatures string           `json:"signatures"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.BaseCommit != base || report.HeadCommit != head || report.Signatures != "sha256_only" {
		t.Fatalf("wrong identities: %+v", report)
	}
	changes := map[string]string{}
	for _, change := range report.Changes {
		changes[change.Name] = change.Kind
		for _, symbol := range append(change.Before, change.After...) {
			if len(symbol.SignatureHash) != 64 {
				t.Fatal("missing signature hash")
			}
		}
	}
	for name, want := range map[string]string{"Body": "source_changed", "Added": "added", "Removed": "removed", "Move": "moved"} {
		if changes[name] != want {
			t.Fatalf("wrong %s classification: %+v", name, changes)
		}
	}
	want := "signature_changed"
	if strings.HasPrefix(graph.Capabilities().Analyzer, "treesitter") {
		want = "source_changed"
	}
	if changes["Signature"] != want {
		t.Fatalf("wrong signature classification: %+v", changes)
	}
	for _, forbidden := range []string{secret, "DirtyMustNotAppear", "UntrackedMustNotAppear", "func Signature"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("uncommitted or raw source leaked: %s", forbidden)
		}
	}
	if got, err := os.ReadFile(index); err != nil || !bytes.Equal(got, original) {
		t.Fatal("persistent graph changed", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, ".git/index")); err != nil || !bytes.Equal(got, indexBefore) {
		t.Fatal("Git index changed", err)
	}
	if compareTestGit(t, root, "rev-parse", "HEAD") != head || compareTestGit(t, root, "show-ref") != refsBefore {
		t.Fatal("Git refs changed")
	}
	if got, err := os.ReadFile(filepath.Join(root, "p.go")); err != nil || !strings.Contains(string(got), "DirtyMustNotAppear") {
		t.Fatal("working source changed", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("repository filter executed", err)
	}
	for _, state := range []string{".trace", ".trace-graph"} {
		if _, err := os.Stat(filepath.Join(root, state)); !os.IsNotExist(err) {
			t.Fatal("comparison created project state", state, err)
		}
	}
}

func TestCommitComparisonRejectsInvalidInputWithoutPartialOutput(t *testing.T) {
	root := t.TempDir()
	compareTestGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte("package p\nfunc Keep(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "add", "-A")
	compareTestGit(t, root, "commit", "-qm", "fixture")
	for _, args := range [][]string{{"compare"}, {"compare", "--base", "--help"}, {"compare", "--base", "missing"}, {"compare", "--base", "HEAD:p.go"}} {
		var output bytes.Buffer
		command := newRoot()
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(append([]string{"--repo", root}, args...))
		if err := command.Execute(); err == nil || output.Len() != 0 {
			t.Fatalf("invalid input published output: %v %v %s", args, err, &output)
		}
	}
}
