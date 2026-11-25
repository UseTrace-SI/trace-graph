package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

// dumpIndex lists every derived row in storage order, so two generations
// compare equal only if they wrote the same rows in the same order.
func dumpIndex(t *testing.T, db *sql.DB) string {
	t.Helper()
	var out strings.Builder
	for _, query := range []string{
		"SELECT rowid,id,file,name,kind,line,COALESCE(signature,''),COALESCE(terms,'') FROM symbols ORDER BY rowid",
		"SELECT rowid,id,from_symbol,to_symbol,kind,file,line FROM relations ORDER BY rowid",
		"SELECT relation_id,from_id,COALESCE(to_id,''),namespace,hint,resolution FROM relation_targets ORDER BY rowid",
		"SELECT symbol_id,namespace FROM symbol_scopes ORDER BY rowid",
		"SELECT symbol_id,source_hash,end_line FROM symbol_content ORDER BY rowid",
		"SELECT symbol_id,receiver FROM symbol_receivers ORDER BY rowid",
		"SELECT path,digest FROM file_cache_v2 ORDER BY rowid",
		"SELECT path,warning FROM file_warnings ORDER BY rowid",
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&out, values...)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out.String()
}

// Parallel parsing must not change what a generation stores or the order
// it is stored in.
func TestParallelIndexIsDeterministic(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n")
	for i := 0; i < 60; i++ {
		write(fmt.Sprintf("p%d/f%d.go", i%6, i), fmt.Sprintf("package p%d\ntype T%d struct{}\nfunc (T%d) M() {}\nfunc F%d() { var t T%d; t.M(); F%d() }\n", i%6, i, i, i, i, (i+6)%60))
		write(fmt.Sprintf("web/m%d.ts", i), fmt.Sprintf("import { f%d } from './m%d'\nexport function f%d() { return f%d() }\n", (i+1)%60, (i+1)%60, i, (i+1)%60))
		write(fmt.Sprintf("py/m%d.py", i), fmt.Sprintf("from .m%d import g%d\n\ndef g%d():\n    return g%d()\n", (i+1)%60, (i+1)%60, i, (i+1)%60))
	}
	for i := 0; i < 6; i++ {
		write(fmt.Sprintf("p%d/a.go", i), fmt.Sprintf("package p%d\n", i))
	}
	write("min.go", "package m\ntype T struct{}\ntype U struct{}\nfunc(T)Same(){};func(U)Same(){}\n")

	original := indexWorkers
	defer func() { indexWorkers = original }()
	var dumps []string
	for _, workers := range []int{1, 8, 3} {
		indexWorkers = func() int { return workers }
		db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := AnalyzeRepo(root, db); err != nil {
			t.Fatal(err)
		}
		dumps = append(dumps, dumpIndex(t, db))
		db.Close()
	}
	if dumps[0] == "" || dumps[0] != dumps[1] || dumps[0] != dumps[2] {
		t.Fatal("generations differ with the number of workers")
	}
	if !strings.Contains(dumps[0], "min.go colliding declaration identities") {
		t.Fatal("collision warning missing from the parallel generation")
	}
}
