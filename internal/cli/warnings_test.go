package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkippedFilesAreReported(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package p\nfunc Fine(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "min.go"), []byte("package p\ntype T struct{}\ntype U struct{}\nfunc(T)Same(){};func(U)Same(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var refresh struct {
		Status  string `json:"status"`
		Skipped []struct {
			Path    string `json:"path"`
			Warning string `json:"warning"`
		} `json:"skipped_files"`
	}
	if err := json.Unmarshal(runContextCLI(t, root, path, "refresh"), &refresh); err != nil || refresh.Status != "ready" || len(refresh.Skipped) != 1 || refresh.Skipped[0].Path != "min.go" {
		t.Fatalf("refresh did not report the skipped file: %+v %v", refresh, err)
	}
	var health struct {
		SkippedFiles int `json:"skipped_files"`
		Skipped      []struct {
			Path string `json:"path"`
		} `json:"skipped"`
		Symbols int `json:"symbols"`
	}
	if err := json.Unmarshal(runContextCLI(t, root, path, "health"), &health); err != nil || health.SkippedFiles != 1 || len(health.Skipped) != 1 || health.Skipped[0].Path != "min.go" || health.Symbols != 1 {
		t.Fatalf("health did not report the skipped file: %+v %v", health, err)
	}
	if out := string(runContextCLI(t, root, path, "analyze")); !strings.Contains(out, "warning: skipped min.go: ") {
		t.Fatalf("analyze did not warn: %s", out)
	}
}
