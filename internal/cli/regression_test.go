package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestRegressionNeighborsOutput(t *testing.T) {
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	path := filepath.Join(t.TempDir(), "g.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature) VALUES('s',?,'main.go','Foo','func',42,'func Foo'); INSERT INTO relations VALUES('r',?,'Caller','Foo','call','caller.go',7); INSERT INTO symbols VALUES('other','other-repo','other.go','Foo','func',99,'func Foo'); INSERT INTO relations VALUES('other','other-repo','OtherCaller','Foo','call','other.go',99)", repo, repo); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--db", path, "--repo", repo, "neighbors", "Foo"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.go:42  func", "Caller  caller.go:7"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "other.go") || strings.Contains(out.String(), "OtherCaller") {
		t.Fatal("neighbor lookup crossed repository boundaries")
	}
}
