package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGraphSnapshotIsScopedVersionedAndDetectablyComplete(t *testing.T) {
	repo, other := t.TempDir(), t.TempDir()
	path := filepath.Join(t.TempDir(), "graph.db")
	run := func(repo string, args ...string) []byte {
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
	for _, r := range []string{repo, other} {
		if err := os.WriteFile(filepath.Join(r, "main.go"), []byte("package p\nfunc A(){}\nfunc B(){A()}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		run(r, "refresh")
	}
	parse := func(data []byte) []map[string]any {
		t.Helper()
		var result []map[string]any
		scan := bufio.NewScanner(bytes.NewReader(data))
		for scan.Scan() {
			var row map[string]any
			if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
			result = append(result, row)
		}
		if err := scan.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	rows := parse(run(repo, "snapshot"))
	if len(rows) != 5 || rows[0]["format"] != "trace.graph/v1" || rows[4]["type"] != "complete" || rows[4]["truncated"] != false || rows[4]["records"] != float64(3) {
		t.Fatalf("invalid snapshot: %+v", rows)
	}
	if rows[1]["name"] != "A" || rows[2]["name"] != "B" || rows[3]["from_symbol"] != "B" {
		t.Fatalf("unexpected graph: %+v", rows)
	}
	bounded := parse(run(repo, "snapshot", "--limit", "1"))
	if len(bounded) != 3 || bounded[2]["truncated"] != true {
		t.Fatalf("silently capped export: %+v", bounded)
	}
	var health map[string]any
	if err := json.Unmarshal(run(repo, "health"), &health); err != nil || health["symbols"] != float64(2) || health["calls"] != float64(1) {
		t.Fatalf("health: %+v %v", health, err)
	}
}
