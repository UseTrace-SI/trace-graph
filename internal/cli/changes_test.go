package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
)

func TestSnapshotChangesDetectSourceSignatureAdditionRemovalAndMovement(t *testing.T) {
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	path := filepath.Join(t.TempDir(), "graph.db")
	base := filepath.Join(t.TempDir(), "before.ndjson")
	run := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		cmd := newRoot()
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--repo", repo, "--db", path}, args...))
		err := cmd.Execute()
		return out.Bytes(), err
	}
	write := func(source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "p.go"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("package p\nfunc Stable(){}\nfunc Body()int{return 1}\nfunc Signature(x int){}\nfunc Removed(){}\nfunc Move(){}\n")
	if _, err := run("refresh"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := run("snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	write("package p\nfunc Stable(){}\nfunc Body()int{return 2}\nfunc Signature(x string){}\nfunc Move(){}\nfunc Added(){}\n")
	if _, err := run("refresh"); err != nil {
		t.Fatal(err)
	}
	data, err := run("changes", "--base", base)
	if err != nil {
		t.Fatalf("%s %v", data, err)
	}
	var result struct {
		Changes []symbolChange `json:"changes"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, change := range result.Changes {
		got[change.Name] = change.Kind
	}
	for name, want := range map[string]string{"Body": "source_changed", "Added": "added", "Removed": "removed", "Move": "moved"} {
		if got[name] != want {
			t.Fatalf("%s: expected %s, got %+v", name, want, got)
		}
	}
	if got["Stable"] != "" {
		t.Fatalf("unchanged symbol changed: %+v", got)
	}
	want := "signature_changed"
	if strings.HasPrefix(graph.Capabilities().Analyzer, "treesitter") {
		want = "source_changed"
	}
	if got["Signature"] != want {
		t.Fatalf("signature: %+v", got)
	}
	if _, err := run("snapshot", "--limit", "1"); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotReaderRejectsIncompleteUnscopedAndCappedInputs(t *testing.T) {
	metadata, _ := json.Marshal(map[string]any{"type": "metadata", "format": "trace.graph/v1", "repo": "repo", "analysis": graph.Capabilities()})
	for _, tail := range []string{"", `{"type":"complete"}`, `{"type":"complete","records":0,"truncated":true}`, `{"type":"complete","records":1,"truncated":false}`, "{bad json}", "{\"type\":\"complete\",\"records\":0,\"truncated\":false}\n{}"} {
		path := filepath.Join(t.TempDir(), "base.ndjson")
		if err := os.WriteFile(path, append(append([]byte{}, metadata...), []byte("\n"+tail+"\n")...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readSnapshot(context.Background(), path, "repo"); err == nil {
			t.Fatalf("accepted invalid snapshot %q", tail)
		}
	}
	path := filepath.Join(t.TempDir(), "base.ndjson")
	valid := string(metadata) + "\n{\"type\":\"complete\",\"records\":0,\"truncated\":false}\n"
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(context.Background(), path, "other-repo"); err == nil {
		t.Fatal("accepted foreign repository")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := readSnapshot(ctx, path, "repo"); err == nil {
		t.Fatal("ignored cancellation")
	}
}
