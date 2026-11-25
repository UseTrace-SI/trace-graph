package graph

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestCompanionIndexExcludesIgnoredAndPrivateState(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	files := map[string]string{
		".gitignore":             "build/\n",
		"main.go":                "package p\nfunc Public() {}\n",
		"build/generated.go":     "package p\nfunc Generated() {}\n",
		".trace-brain/secret.go": "package p\nfunc PrivateBrain() {}\n",
		".trace-graph/secret.go": "package p\nfunc PrivateGraph() {}\n",
		".env.go":                "package p\nfunc SecretEnv() {}\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", root, "add", "-f", ".trace-brain/secret.go", ".env.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %s %v", out, err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM symbols").Scan(&n); err != nil || n != 1 {
		t.Fatalf("private or generated sources indexed: %d %v", n, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("build/\nmain.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM symbols").Scan(&n); err != nil || n != 0 {
		t.Fatalf("newly ignored source remained indexed: %d %v", n, err)
	}
}

func TestProjectExclusionsRemoveDerivedSymbols(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"keep.go": "package p\nfunc Keep(){}\n", "private.go": "package p\nfunc Private(){}\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".traceignore"), []byte("private.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name='Private'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("excluded code still indexed: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM symbols WHERE name='Keep'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("unrelated code removed: %d %v", n, err)
	}
}

// A directory without Git has no .gitignore, so build output, virtual
// environments and minified bundles are excluded by name. A Git checkout
// keeps indexing a tracked build/ directory.
func TestNonGitDirectoryExcludesBuildOutput(t *testing.T) {
	files := map[string]string{
		"main.go":                     "package p\nfunc Keep(){}\n",
		"package.json":                "{}\n",
		"src/app.js":                  "function app(){}\n",
		"dist/bundle.js":              "function bundled(){}\n",
		"build/gen.go":                "package p\nfunc Built(){}\n",
		"target/gen.rs":               "fn built(){}\n",
		".venv/lib/site.py":           "def venv():\n    pass\n",
		"web/node_modules/m/index.js": "function dep(){}\n",
		"web/vendor.min.js":           "function minified(){}\n",
		"pkg/builder/b.go":            "package builder\nfunc Builder(){}\n",
		// Deeper directories with those names are packages (go/build,
		// cmd/dist) unless a build manifest sits beside them.
		"internal/build/b.go":   "package build\nfunc InternalBuild(){}\n",
		"cmd/dist/d.go":         "package main\nfunc CmdDist(){}\n",
		"cmd/target/t.go":       "package target\nfunc CmdTarget(){}\n",
		"web/package.json":      "{}\n",
		"web/dist/app.js":       "function webDist(){}\n",
		"crate/Cargo.toml":      "[package]\n",
		"crate/target/gen.rs":   "fn crate_target(){}\n",
		"py/sub/.venv/lib/x.py": "def nested_venv():\n    pass\n",
	}
	write := func(root string) {
		for rel, content := range files {
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := func(root string) []string {
		db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := AnalyzeRepo(root, db); err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query("SELECT name FROM symbols ORDER BY name")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		return got
	}
	plain := t.TempDir()
	write(plain)
	if got := strings.Join(names(plain), ","); got != "Builder,CmdDist,CmdTarget,InternalBuild,Keep,app" {
		t.Fatalf("non-Git directory indexed build output: %s", got)
	}
	checkout := t.TempDir()
	write(checkout)
	if out, err := exec.Command("git", "-C", checkout, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	if got := strings.Join(names(checkout), ","); !strings.Contains(got, "Built") {
		t.Fatalf("Git checkout lost an unignored build/ source: %s", got)
	}
}

// Without a build manifest beside them, dist/, build/ and target/ are
// packages, as in GOROOT/src/cmd (a Go module whose dist/ is cmd/dist).
func TestNonGitDirectoryWithoutManifestKeepsDistPackages(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"go.mod":        "module cmd\n",
		"dist/main.go":  "package main\nfunc DistMain(){}\n",
		"build/b.go":    "package build\nfunc BuildPkg(){}\n",
		"vendor.min.js": "function minified(){}\n",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var names []string
	rows, err := db.Query("SELECT name FROM symbols ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if got := strings.Join(names, ","); got != "BuildPkg,DistMain" {
		t.Fatalf("symbols: %s", got)
	}
}
