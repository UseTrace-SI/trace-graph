package cli

import (
	"bytes"
	"context"
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

// Hooks export GIT_DIR and GIT_WORK_TREE. cochange inherited them and
// reported another repository's history as this repository's coupling.
func TestRegressionCochangeIgnoresAmbientGitRedirection(t *testing.T) {
	history := func(files ...string) string {
		t.Helper()
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		compareTestGit(t, root, "init", "-q")
		for i := 0; i < 2; i++ {
			for _, file := range files {
				if err := os.WriteFile(filepath.Join(root, file), []byte(fmt.Sprintf("package p\n// %d\n", i)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			compareTestGit(t, root, "add", "-A")
			compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
		}
		return root
	}
	root := history("a.go", "b.go")
	foreign := history("foreign_c.go", "foreign_d.go")
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--repo", root, "cochange"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s: %v", &out, err)
	}
	if !strings.Contains(out.String(), "a.go  <->  b.go") || strings.Contains(out.String(), "foreign") {
		t.Fatalf("cochange read the wrong repository: %s", &out)
	}
}

// git log quoted non-ASCII and special-character paths ("\303\274ber.go"),
// so they failed the source filter and vanished from cochange.
func TestRegressionCochangeKeepsQuotedPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	for i := 0; i < 2; i++ {
		for _, file := range []string{"über.go", `back\slash.go`, "plain.go"} {
			if err := os.WriteFile(filepath.Join(root, file), []byte(fmt.Sprintf("package p\n// %d\n", i)), 0600); err != nil {
				t.Fatal(err)
			}
		}
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
	}
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--repo", root, "cochange"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s: %v", &out, err)
	}
	for _, want := range []string{"2  plain.go  <->  über.go", `2  back\slash.go  <->  plain.go`, `2  back\slash.go  <->  über.go`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, &out)
		}
	}
}

// The pair budget refused a history once it held exactly --max-pairs unique
// pairs and any of them changed again, although no new pair was added.
func TestRegressionCochangePairBudgetCountsUniquePairs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		commits [][]string
		want    string
		fails   bool
	}{
		{name: "repeated pair stays within budget", commits: [][]string{{"a.go", "b.go"}, {"a.go", "b.go"}, {"a.go", "b.go"}}, want: "3  a.go  <->  b.go"},
		{name: "new pair exceeds budget", commits: [][]string{{"a.go", "b.go"}, {"c.go", "d.go"}}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			compareTestGit(t, root, "init", "-q")
			for i, files := range tc.commits {
				for _, file := range files {
					if err := os.WriteFile(filepath.Join(root, file), []byte(fmt.Sprintf("package p\n// %d\n", i)), 0600); err != nil {
						t.Fatal(err)
					}
				}
				compareTestGit(t, root, "add", "-A")
				compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
			}
			var out bytes.Buffer
			cmd := newRoot()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--repo", root, "cochange", "--max-pairs", "1"})
			err = cmd.Execute()
			if tc.fails {
				if err == nil || !strings.Contains(err.Error(), "exceeds 1 unique file pairs") {
					t.Fatalf("budget not enforced: %v %s", err, &out)
				}
				return
			}
			if err != nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("got %v %q, want %q", err, &out, tc.want)
			}
		})
	}
}

// Configuration files are not indexed, but their co-change history with
// sources is useful: cochange pairs YAML, JSON, Dockerfiles and the like,
// while secret paths and lockfiles stay out.
func TestCochangeIncludesConfigurationFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	for i := 0; i < 2; i++ {
		for name, text := range map[string]string{
			"deploy.yaml":       fmt.Sprintf("replicas: %d\n", i),
			"cfg.yml":           fmt.Sprintf("level: %d\n", i),
			"m.go":              fmt.Sprintf("package m\nfunc M%d(){}\n", i),
			"Dockerfile":        fmt.Sprintf("FROM scratch\n# %d\n", i),
			"package-lock.json": fmt.Sprintf("{\"v\": %d}\n", i),
			".env.yaml":         fmt.Sprintf("secret: %d\n", i),
			// Credential files the Trace CLI classifies as secrets, and a
			// .traceignored config, are never named.
			"credentials.json":     fmt.Sprintf("{\"k\": %d}\n", i),
			"service-account.json": fmt.Sprintf("{\"k\": %d}\n", i),
			"kubeconfig.yaml":      fmt.Sprintf("token: %d\n", i),
			"ignored.yaml":         fmt.Sprintf("x: %d\n", i),
			".traceignore":         "ignored.yaml\n",
		} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
	}
	pairs, err := cochangePairs(context.Background(), root, 10, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range pairs {
		got = append(got, p.A+"+"+p.B)
	}
	text := strings.Join(got, " ")
	for _, want := range []string{"cfg.yml+deploy.yaml", "deploy.yaml+m.go", "Dockerfile+m.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	for _, never := range []string{"package-lock.json", ".env", "credentials", "service-account", "kubeconfig", "ignored.yaml"} {
		if strings.Contains(text, never) {
			t.Fatalf("%s paired: %s", never, text)
		}
	}
}

// With --repo selecting a subdirectory, git log prints paths relative to the
// working-tree root. The root's .traceignore (and the subdirectory's own)
// applies, pairs stay inside the subdirectory and paths are shown relative
// to it.
func TestCochangeInSubdirectoryHonorsPolicyAndScope(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		for name, text := range map[string]string{
			".traceignore":     "sub/secret.go\n",
			"sub/.traceignore": "local.go\n",
			"sub/a.go":         fmt.Sprintf("package s\nfunc A%d(){}\n", i),
			"sub/b.go":         fmt.Sprintf("package s\nfunc B%d(){}\n", i),
			"sub/secret.go":    fmt.Sprintf("package s\nfunc S%d(){}\n", i),
			"sub/local.go":     fmt.Sprintf("package s\nfunc L%d(){}\n", i),
			"top.go":           fmt.Sprintf("package t\nfunc T%d(){}\n", i),
		} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
	}
	pairs, err := cochangePairs(context.Background(), filepath.Join(root, "sub"), 10, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].A != "a.go" || pairs[0].B != "b.go" || pairs[0].Count != 2 {
		t.Fatalf("subdirectory pairs: %+v", pairs)
	}
	pairs, err = cochangePairs(context.Background(), root, 10, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range pairs {
		got = append(got, p.A+"+"+p.B)
	}
	if text := strings.Join(got, " "); strings.Contains(text, "secret.go") || !strings.Contains(text, "sub/a.go+sub/b.go") || !strings.Contains(text, "sub/local.go") {
		t.Fatalf("root pairs: %s", text)
	}
}

// diff.relative=true makes git log print paths relative to the current
// directory, which CochangeScope (expecting working-tree-root paths) then
// rejected, so a subdirectory --repo returned nothing.
func TestRegressionCochangeIgnoresDiffRelativeConfig(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	compareTestGit(t, root, "init", "-q")
	compareTestGit(t, root, "config", "diff.relative", "true")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		for name, text := range map[string]string{
			"sub/a.go": fmt.Sprintf("package s\nfunc A%d(){}\n", i),
			"sub/b.go": fmt.Sprintf("package s\nfunc B%d(){}\n", i),
			"top.go":   fmt.Sprintf("package t\nfunc T%d(){}\n", i),
		} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		compareTestGit(t, root, "add", "-A")
		compareTestGit(t, root, "commit", "-qm", fmt.Sprintf("change %d", i))
	}
	pairs, err := cochangePairs(context.Background(), filepath.Join(root, "sub"), 10, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].A != "a.go" || pairs[0].B != "b.go" || pairs[0].Count != 2 {
		t.Fatalf("subdirectory pairs with diff.relative=true: %+v", pairs)
	}
	pairs, err = cochangePairs(context.Background(), root, 10, 1000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 3 {
		t.Fatalf("root pairs with diff.relative=true: %+v", pairs)
	}
}
