package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
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

// A deep graph used to hit the hard two-second deadline inside BFS and fail
// outright. The walk must degrade to a best-effort report when its budget is
// exhausted instead of returning an error.
func TestTransitiveImpactDeadlineDegradesOnDeepGraph(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO repo_state VALUES(?,123)", repo); err != nil {
		t.Fatal(err)
	}
	const nodes = 60
	name := func(i int) string { return fmt.Sprintf("S%02d", i) }
	id := func(i int) string { return repo + name(i) }
	for i := 0; i < nodes; i++ {
		if _, err = db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)", id(i), repo, "chain.go", name(i), "func", i+1, "func S()"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < nodes-1; i++ {
		rid := fmt.Sprintf("%srel%02d", repo, i)
		if _, err = db.Exec("INSERT INTO relations VALUES(?,?,?,?,?,?,?)", rid, repo, name(i), name(i+1), "call", "chain.go", i+1); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("INSERT INTO relation_targets VALUES(?,?,?,?,?,?)", rid, id(i), id(i+1), "p", "identifier", "resolved"); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	rdb, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	tx, err := rdb.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var root codeSymbol
	if err := tx.QueryRow("SELECT id,file,name,kind,line,substr(COALESCE(signature,''),1,2048) FROM symbols WHERE repo=? AND name=?", repo, name(0)).Scan(&root.ID, &root.File, &root.Name, &root.Kind, &root.Line, &root.Signature); err != nil {
		t.Fatal(err)
	}

	// With budget to spare, the deep chain is bounded by depth, not by time.
	healthy := impactReport{Symbol: root, Repo: repo, Direction: "callees", Nodes: []impactNode{}}
	if err := expandImpact(context.Background(), tx, &healthy, repo, "callees", 20, 1000); err != nil {
		t.Fatal(err)
	}
	if healthy.DeadlineExceeded || !healthy.Truncated || len(healthy.Nodes) != 20 {
		t.Fatalf("deep chain was not bounded by depth: %+v", healthy)
	}

	// With the budget already exhausted, the same graph must return a
	// best-effort report carrying the deadline flag rather than an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expired := impactReport{Symbol: root, Repo: repo, Direction: "callees", Nodes: []impactNode{}}
	if err := expandImpact(ctx, tx, &expired, repo, "callees", 20, 1000); err != nil {
		t.Fatalf("exhausted budget caused a hard failure: %v", err)
	}
	if !expired.DeadlineExceeded || !expired.Truncated {
		t.Fatalf("exhausted budget was not reported: %+v", expired)
	}
}

func TestGraphInspectionNeverCreatesDatabase(t *testing.T) {
	for _, inspect := range [][]string{{"health"}, {"impact", "A"}, {"symbols"}, {"query", "A"}, {"neighbors", "A"}, {"def", "A"}} {
		path := filepath.Join(t.TempDir(), "missing.db")
		cmd := newRoot()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--repo", t.TempDir(), "--db", path}, inspect...))
		if err := cmd.Execute(); err == nil {
			t.Fatal("missing index was accepted")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("inspection created a database")
		}
	}
}

// Interface calls stay unresolved call relations. Their possible module
// implementations are dispatch edges, which impact follows only on request,
// and health counts calls separately from type relationships.
func TestImpactAndHealthSeparateCallsFromTypeRelationships(t *testing.T) {
	if !strings.HasPrefix(graph.Capabilities().Analyzer, "inventory") {
		t.Skip("package-wide Go type checking runs in the default build")
	}
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(t.TempDir(), "graph.db")
	src := "package p\n\ntype Store interface{ Get() string }\n\ntype Memory struct{}\n\nfunc (Memory) Get() string { return \"\" }\n\nfunc Handle(s Store) string { return s.Get() }\n\nfunc Direct() string { return Memory{}.Get() }\n"
	if err := os.WriteFile(filepath.Join(repo, "p.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--repo", repo, "--db", path}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v %s", args, err, &out)
		}
		return out.Bytes()
	}
	run("refresh")
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"impact", "Get", "--json"}, []string{"Direct"}},
		{[]string{"impact", "Get", "--json", "--include-dispatch"}, []string{"Handle", "Direct"}},
	} {
		var report impactReport
		if err := json.Unmarshal(run(tc.args...), &report); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, node := range report.Nodes {
			names = append(names, node.Name)
		}
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%v: callers %v, want %v", tc.args, names, tc.want)
		}
	}
	var health struct {
		Calls          int            `json:"calls"`
		Resolved       int            `json:"resolved_calls"`
		CallResolution map[string]int `json:"call_resolution"`
		Relations      map[string]int `json:"relations"`
		GoTypes        struct {
			Status string `json:"status"`
		} `json:"go_types"`
	}
	if err := json.Unmarshal(run("health"), &health); err != nil {
		t.Fatal(err)
	}
	if health.Calls != 2 || health.Resolved != 1 || health.CallResolution["typed"] != 1 || health.CallResolution["dynamic"] != 1 ||
		health.Relations["call"] != 2 || health.Relations["implements"] != 1 || health.Relations["dispatch"] != 1 || health.GoTypes.Status != "complete" {
		t.Fatalf("health mixed calls and type relationships: %+v", health)
	}
}
