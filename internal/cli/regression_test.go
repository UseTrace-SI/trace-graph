package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestRegressionNeighborsOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO symbols(id,repo,file,name,kind,line,signature) VALUES('s','r','main.go','Foo','func',42,'func Foo'); INSERT INTO relations VALUES('r','r','Caller','Foo','call','caller.go',7)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--db", path, "neighbors", "Foo"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.go:42  func", "Caller  caller.go:7"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
}
