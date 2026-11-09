//go:build !treesitter

package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestReceiverResolutionUsesExplicitTypesAndLeavesDynamicCallsUnresolved(t *testing.T) {
	repo := t.TempDir()
	source := `package p
type T struct{}
type U struct{}
type Alias = T
type I interface { Run() }
func (T) Run(){}
func (U) Run(){}
func Explicit(t T,u U,p *T){t.Run();u.Run();p.Run();T.Run(t)}
func Unresolved(i I,a Alias){i.Run();a.Run();inferred:=T{};inferred.Run()}
func Generic[T interface{Run()}](value T){value.Run()}
`
	if err := os.WriteFile(filepath.Join(repo, "p.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(repo, db); err != nil {
		t.Fatal(err)
	}
	var resolved, unresolved int
	if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE to_id IS NOT NULL").Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM relation_targets WHERE to_id IS NULL").Scan(&unresolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 4 || unresolved != 4 {
		t.Fatalf("guessed receiver edges: %d resolved, %d unresolved", resolved, unresolved)
	}
	var wrong int
	if err := db.QueryRow(`SELECT count(*) FROM relations r JOIN relation_targets t ON t.relation_id=r.id JOIN symbol_receivers s ON s.symbol_id=t.to_id WHERE r.from_symbol='Explicit' AND r.line=8 AND s.receiver NOT IN ('T','U')`).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("wrong receiver: %d %v", wrong, err)
	}
	if err := os.Remove(filepath.Join(repo, "p.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(repo, db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"symbol_receivers", "symbol_content"} {
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&wrong); err != nil || wrong != 0 {
			t.Fatalf("stale %s: %d %v", table, wrong, err)
		}
	}
}
