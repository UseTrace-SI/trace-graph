package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/embed"
	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func runContextCLI(t *testing.T, repo, dbPath string, args ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--repo", repo, "--db", dbPath}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, &out)
	}
	return out.Bytes()
}

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

// A supplied --path used to skip the FTS branch entirely, silently discarding
// --query. FTS MATCH and the file/repository filters must be applied together.
func TestContextPathAndQueryAreCombined(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	for file, src := range map[string]string{
		"a.go": "package p\nfunc Alpha(){}\nfunc Shared(){}\n",
		"b.go": "package p\nfunc Beta(){}\nfunc Shared2(){}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runContextCLI(t, root, path, "refresh")
	for _, tc := range []struct {
		file, query string
		want        int
		name        string
	}{
		{"a.go", "Alpha", 1, "Alpha"},
		{"a.go", "Beta", 0, ""},
		{"b.go", "Beta", 1, "Beta"},
	} {
		var got codeContext
		out := runContextCLI(t, root, path, "context", "--path", tc.file, "--query", tc.query)
		if err := json.Unmarshal(out, &got); err != nil || got.Status != "ready" {
			t.Fatalf("%s %s: %+v %v %s", tc.file, tc.query, got, err, out)
		}
		if len(got.Symbols) != tc.want {
			t.Fatalf("%s %q: %d symbols, want %d (%s)", tc.file, tc.query, len(got.Symbols), tc.want, out)
		}
		if tc.want > 0 && got.Symbols[0].Name != tc.name {
			t.Fatalf("%s %q: got %q, want %q", tc.file, tc.query, got.Symbols[0].Name, tc.name)
		}
	}
}

// Semantic ranking must stay bounded on a large index and return the best
// candidates seen instead of failing when the two-second deadline expires.
func TestContextSemanticLargeIndexIsBounded(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc Seed(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runContextCLI(t, root, path, "refresh")

	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	symbols, err := tx.Prepare("INSERT INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,'synthetic.go',?,'func',?,'func '||?)")
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := tx.Prepare("INSERT INTO symbol_embeddings(symbol_id,dim,vector) VALUES(?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	vector := embed.Encode(embed.Embed("Alpha common"))
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("synthetic:%d", i)
		name := fmt.Sprintf("Alpha%04d", i)
		if _, err := symbols.Exec(id, root, name, i+1, name); err != nil {
			t.Fatal(err)
		}
		if _, err := vectors.Exec(id, embed.Dim, vector); err != nil {
			t.Fatal(err)
		}
	}
	symbols.Close()
	vectors.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var got codeContext
	out := runContextCLI(t, root, path, "context", "--semantic", "--query", "Alpha", "--limit", "5")
	if err := json.Unmarshal(out, &got); err != nil || got.Status != "ready" {
		t.Fatalf("%+v %v %s", got, err, out)
	}
	if len(got.Symbols) == 0 || len(got.Symbols) > 5 {
		t.Fatalf("semantic scan returned %d symbols", len(got.Symbols))
	}
}
