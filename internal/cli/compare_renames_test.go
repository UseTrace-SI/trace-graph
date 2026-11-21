package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
)

func TestCommitComparisonExactRenamesPreserveAmbiguityAndExclusions(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	write := func(file, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", "fixture")
		return compareTestGit(t, root, "rev-parse", "HEAD")
	}
	move := func(from, to string) {
		t.Helper()
		if err := os.Rename(filepath.Join(root, from), filepath.Join(root, to)); err != nil {
			t.Fatal(err)
		}
	}
	moved := "package p\nfunc Moved()int{return 1}\n"
	write("old.go", moved)
	write("empty-old.go", "package p\n// no symbols\n")
	for _, file := range []string{"dup-a.go", "dup-b.go"} {
		write(file, "package p\nfunc Duplicate(){}\n")
	}
	for _, file := range []string{"retained.go", "removed.go"} {
		write(file, "package p\nfunc RetainedDuplicate(){}\n")
	}
	write("keep.go", "package p\nfunc Copied(){}\n")
	write("edited-old.go", "package p\nfunc Edited()int{return 1}\n")
	write("extension.go", "package p\nfunc DifferentParser(){}\n")
	write("survives.go", "package p\nfunc PathSurvives(){}\n")
	write("directory.go", "package p\nfunc DirectoryReplaces(){}\n")
	write("private-old.go", "package p\nfunc ExcludedBefore(){}\n")
	write(".traceignore", "private-old.go\n")
	base := commit()
	move("old.go", "new.go")
	move("empty-old.go", "empty-new.go")
	move("dup-a.go", "dup-c.go")
	move("dup-b.go", "dup-d.go")
	move("removed.go", "added.go")
	write("copy.go", "package p\nfunc Copied(){}\n")
	move("edited-old.go", "edited-new.go")
	write("edited-new.go", "package p\nfunc Edited()int{return 2}\n")
	move("extension.go", "extension.js")
	move("survives.go", "relocated.go")
	if err := os.Symlink("relocated.go", filepath.Join(root, "survives.go")); err != nil {
		t.Fatal(err)
	}
	move("directory.go", "relocated-directory.go")
	write("directory.go/note.txt", "a directory now occupies this path\n")
	move("private-old.go", "public-new.go")
	head := commit()
	dirty := "package p\nfunc DirtyMustNotAppear(){}\n"
	write("new.go", dirty)
	indexBefore, err := os.ReadFile(filepath.Join(root, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	refsBefore := compareTestGit(t, root, "show-ref")
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		command := newRoot()
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		args := []string{"--repo", root, "compare", "--base", base, "--head", head}
		if enabled {
			args = append(args, "--detect-renames")
		}
		command.SetArgs(args)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		var report struct {
			Mode      string                  `json:"rename_detection"`
			Renames   []graph.FileRename      `json:"file_renames"`
			Ambiguous []graph.RenameAmbiguity `json:"rename_ambiguities"`
			Changes   []revisionChange        `json:"changes"`
		}
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), "private-old.go") || strings.Contains(output.String(), "DirtyMustNotAppear") {
			t.Fatal("excluded or dirty data appeared in report")
		}
		movedChanges := []revisionChange{}
		for _, change := range report.Changes {
			if change.Name == "Moved" {
				movedChanges = append(movedChanges, change)
			} else if change.Kind == "file_moved" {
				t.Fatalf("guessed relocation: %+v", change)
			}
		}
		if enabled {
			if report.Mode != "unique_exact_content_same_extension" || len(report.Renames) != 2 || len(report.Ambiguous) != 2 || len(movedChanges) != 1 {
				t.Fatalf("wrong relocation report: %+v", report)
			}
			change := movedChanges[0]
			if change.Kind != "file_moved" || change.Before[0].File != "old.go" || change.After[0].File != "new.go" {
				t.Fatalf("lost original symbol locations: %+v", change)
			}
			if report.Renames[1].SourceHash != fmt.Sprintf("%x", sha256.Sum256([]byte(moved))) {
				t.Fatal("file hash does not match committed bytes")
			}
			for _, ambiguity := range report.Ambiguous {
				if ambiguity.BaseMatches != 2 || ambiguity.HeadMatches != 2 {
					t.Fatalf("duplicate content not counted: %+v", ambiguity)
				}
			}
		} else if report.Mode != "disabled" || len(report.Renames) != 0 || len(report.Ambiguous) != 0 || len(movedChanges) != 2 || movedChanges[0].Kind == "file_moved" {
			t.Fatalf("default comparison changed: %+v", report)
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "new.go")); err != nil || string(got) != dirty {
		t.Fatal("dirty source changed", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, ".git/index")); err != nil || !bytes.Equal(indexBefore, got) {
		t.Fatal("Git index changed", err)
	}
	if compareTestGit(t, root, "show-ref") != refsBefore || compareTestGit(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("Git refs changed")
	}
}

// With --detect-renames, a removed and an added symbol of the same kind
// whose name-normalized bodies agree become one "renamed" change, reported
// with evidence and confidence; unrelated removals and additions stay apart
// and the default comparison is unchanged.
func TestCommitComparisonPairsRenamedSymbolsWithConfidence(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	write := func(src string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", "fixture")
		return compareTestGit(t, root, "rev-parse", "HEAD")
	}
	total := func(name, limit, extra string) string {
		return "func " + name + "(a, b int) int {\n\tsum := a + b\n\tif sum > " + limit + " {\n\t\treturn sum - " + limit + "\n\t}\n" + extra + "\treturn sum * 2\n}\n"
	}
	base := write("package p\n\n" + total("Total", "10", "") + total("Score", "99", "") + "func Gone() string { return \"gone\" }\n")
	head := write("package p\n\n" + total("Sum", "10", "") + total("Rank", "99", "\tsum++\n") + "func Fresh(path string) error { return nil }\n")
	run := func(args ...string) map[string]any {
		t.Helper()
		var output bytes.Buffer
		command := newRoot()
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(append([]string{"--repo", root, "compare", "--base", base, "--head", head}, args...))
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		var report map[string]any
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	kinds := func(report map[string]any) map[string]string {
		got := map[string]string{}
		for _, raw := range report["changes"].([]any) {
			change := raw.(map[string]any)
			got[change["name"].(string)] = change["kind"].(string)
		}
		return got
	}
	plain := run()
	if got := kinds(plain); got["Total"] != "removed" || got["Sum"] != "added" || plain["symbol_rename_detection"] != "disabled" || len(plain["symbol_renames"].([]any)) != 0 {
		t.Fatalf("default comparison changed: %v %v", got, plain["symbol_rename_detection"])
	}
	detected := run("--detect-renames")
	got := kinds(detected)
	for name, want := range map[string]string{"Sum": "renamed", "Rank": "renamed", "Gone": "removed", "Fresh": "added"} {
		if got[name] != want {
			t.Fatalf("%s: %s, want %s (%v)", name, got[name], want, got)
		}
	}
	if _, ok := got["Total"]; ok {
		t.Fatalf("renamed symbol also reported under its old name: %v", got)
	}
	evidence := map[string]string{}
	for _, raw := range detected["symbol_renames"].([]any) {
		rename := raw.(map[string]any)
		from, to := rename["from"].(map[string]any), rename["to"].(map[string]any)
		confidence := rename["confidence"].(float64)
		evidence[from["name"].(string)+"->"+to["name"].(string)] = rename["evidence"].(string)
		if (rename["evidence"] == "identical_body" && confidence != 1) || confidence < 0.7 || len(to["source_hash"].(string)) != 64 {
			t.Fatalf("unexpected rename evidence: %v", rename)
		}
	}
	if evidence["Total->Sum"] != "identical_body" || evidence["Score->Rank"] != "similar_body" || len(evidence) != 2 || detected["symbol_rename_detection"] != "name_normalized_body" {
		t.Fatalf("renames %v (%v)", evidence, detected["symbol_rename_detection"])
	}
	if strings.Contains(fmt.Sprint(detected), "sum * 2") {
		t.Fatal("raw source leaked into the comparison report")
	}
}
