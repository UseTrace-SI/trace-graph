package graph

import (
	"os"
	"os/exec"
	"path/filepath"
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
