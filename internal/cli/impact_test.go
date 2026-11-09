package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestTransitiveImpactIdentityCyclesLimitsAndIsolation(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{repo, "other-repo"} {
		if _, err = db.Exec("INSERT INTO repo_state VALUES(?,123)", r); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"A", "B", "C", "D", "Unused"} {
			if _, err = db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)", r+name, r, name+".go", name, "func", 1, "func "+name+"()"); err != nil {
				t.Fatal(err)
			}
		}
		for i, edge := range [][2]string{{"A", "B"}, {"B", "C"}, {"C", "A"}, {"D", "A"}} {
			id := fmt.Sprintf("%s%d", r, i)
			if _, err = db.Exec("INSERT INTO relations VALUES(?,?,?,?,?,?,?)", id, r, edge[0], edge[1], "call", edge[0]+".go", 1); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO relation_targets VALUES(?,?,?,?,?,?)", id, r+edge[0], r+edge[1], "p", "identifier", "lexical"); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.Close()
	run := func(args ...string) (impactReport, error) {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--repo", repo, "--db", path, "impact", "A", "--json"}, args...))
		err := cmd.Execute()
		var result impactReport
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &result)
		}
		return result, err
	}
	for _, tc := range []struct {
		direction string
		want      int
	}{{"callers", 3}, {"callees", 2}, {"both", 3}} {
		got, err := run("--direction", tc.direction, "--depth", "20")
		if err != nil || len(got.Nodes) != tc.want || got.Truncated || got.IndexedAt != 123 {
			t.Fatalf("%s: %+v %v", tc.direction, got, err)
		}
		seen := map[string]bool{repo + "A": true}
		for _, n := range got.Nodes {
			if !strings.HasPrefix(n.ID, repo) || seen[n.ID] || n.Via == "" {
				t.Fatalf("cross-repo or cyclic output: %+v", got)
			}
			seen[n.ID] = true
		}
	}
	got, err := run("--depth", "1")
	if err != nil || len(got.Nodes) != 2 || !got.Truncated {
		t.Fatalf("depth: %+v %v", got, err)
	}
	got, err = run("--limit", "1")
	if err != nil || len(got.Nodes) != 1 || !got.Truncated {
		t.Fatalf("limit: %+v %v", got, err)
	}
	for _, args := range [][]string{{"--depth", "0"}, {"--depth", "21"}, {"--limit", "0"}, {"--direction", "guess"}} {
		if _, err = run(args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO symbols VALUES(?,?,?,?,?,?,?)", "duplicate", repo, "dup.go", "A", "func", 1, "func A()")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("guessed ambiguous target: %v", err)
	}
}

func TestGraphInspectionNeverCreatesDatabase(t *testing.T) {
	for _, name := range []string{"health", "impact"} {
		path := filepath.Join(t.TempDir(), "missing.db")
		cmd := newRoot()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		args := []string{"--repo", t.TempDir(), "--db", path, name}
		if name == "impact" {
			args = append(args, "A")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("missing index was accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("inspection created a database")
		}
	}
}
