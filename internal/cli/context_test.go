package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCompanionContextIsScopedAndReadOnly(t *testing.T) {
	root, other, dir := t.TempDir(), t.TempDir(), t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	other, _ = filepath.EvalSymlinks(other)
	path := filepath.Join(dir, "graph.db")
	run := func(repo string, args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--repo", repo, "--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, &out)
		}
		return out.Bytes()
	}
	var got codeContext
	if err := json.Unmarshal(run(root, "context", "--path", "main.go"), &got); err != nil || got.Status != "missing_index" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("lookup created a database")
	}
	for _, repo := range []string{root, other} {
		if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package p\nfunc Validate() bool{return true}\nfunc Login() bool{return Validate()}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		run(repo, "refresh")
	}
	if err := json.Unmarshal(run(root, "context", "--query", "Login()"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Protocol != 1 || got.Status != "ready" || got.Repo != root || got.IndexedAt == 0 || len(got.Symbols) != 1 || len(got.Relations) != 1 {
		t.Fatalf("cross-repo or missing context: %+v", got)
	}
	if got.Relations[0].From != "Login" || got.Relations[0].To != "Validate" {
		t.Fatalf("bad relation: %+v", got)
	}
	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	run(root, "refresh")
	if err := json.Unmarshal(run(root, "context", "--path", "main.go"), &got); err != nil || len(got.Symbols) != 0 || len(got.Relations) != 0 {
		t.Fatalf("deleted source leaked: %+v %v", got, err)
	}
	if err := json.Unmarshal(run(other, "context", "--path", "main.go"), &got); err != nil || len(got.Symbols) != 2 {
		t.Fatalf("other repository damaged: %+v %v", got, err)
	}
}
