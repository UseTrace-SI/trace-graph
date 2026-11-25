package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWarningsAreReported(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package p\nfunc Fine(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "min.go"), []byte("package p\ntype T struct{}\ntype U struct{}\nfunc(T)Same(){};func(U)Same(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var refresh struct {
		Status   string `json:"status"`
		Warnings []struct {
			Path    string `json:"path"`
			Warning string `json:"warning"`
		} `json:"file_warnings"`
	}
	if err := json.Unmarshal(runContextCLI(t, root, path, "refresh"), &refresh); err != nil || refresh.Status != "ready" || len(refresh.Warnings) != 1 || refresh.Warnings[0].Path != "min.go" {
		t.Fatalf("refresh did not report the file warning: %+v %v", refresh, err)
	}
	var health struct {
		Warned   int `json:"files_with_warnings"`
		Warnings []struct {
			Path string `json:"path"`
		} `json:"file_warnings"`
		Symbols int `json:"symbols"`
	}
	if err := json.Unmarshal(runContextCLI(t, root, path, "health"), &health); err != nil || health.Warned != 1 || len(health.Warnings) != 1 || health.Warnings[0].Path != "min.go" || health.Symbols != 4 {
		t.Fatalf("health did not report the file warning: %+v %v", health, err)
	}
	if out := string(runContextCLI(t, root, path, "analyze")); !strings.Contains(out, "warning: min.go: colliding declaration identities") {
		t.Fatalf("analyze did not warn: %s", out)
	}
}

// The index holds source-derived symbols and signatures, so a database
// directory created by Graph is private to its owner.
func TestDefaultDatabaseDirectoryIsPrivate(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc A(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newRoot()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--repo", root, "refresh"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, ".trace-graph"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Fatalf("database directory mode %o, want 700", perm)
	}
}
