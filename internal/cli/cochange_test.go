package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCochangeIsBoundedAndFiltered(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	write := func(name, text string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		write("a.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		write("b.go", fmt.Sprintf("package p\nfunc B%d(){}\n", i))
		write("notes.md", "not a supported source")
		write(filepath.Join(".env", "secret.go"), "package p\nfunc Secret(){}\n")
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
	}

	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--repo", root, "cochange", "--max-count", "10"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s: %v", &out, err)
	}
	text := out.String()
	var paired bool
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "a.go") && strings.Contains(line, "b.go") {
			paired = true
		}
	}
	if !paired {
		t.Fatalf("co-changing sources missing: %s", text)
	}
	if strings.Contains(text, "notes.md") || strings.Contains(text, "secret.go") {
		t.Fatalf("unsupported or excluded path reported: %s", text)
	}

	for _, args := range [][]string{
		{"--max-count", "0"},
		{"--max-pairs", "0"},
		{"--max-log-bytes", "1"},
	} {
		var bad bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&bad)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--repo", root, "cochange"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted invalid bounds %v", args)
		}
	}
}

// A single large commit (for example an initial import) must not make the
// default cochange invocation fail just because its per-commit file cap is
// larger than the pair budget allows. 460 files produce 105570 unique pairs,
// above the default --max-pairs of 100000.
func TestCochangeLargeCommitWithinDefaultPairBudget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	const files = 460
	for i := 0; i < files; i++ {
		path := filepath.Join(root, fmt.Sprintf("src/f%03d.go", i))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package p\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	compareTestGit(t, root, "add", "-A")
	compareTestGit(t, root, "commit", "-qm", "large import")

	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--repo", root, "cochange"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("default cochange failed on a large commit: %v\n%s", err, &out)
	}
}
