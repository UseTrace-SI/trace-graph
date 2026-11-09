//go:build !treesitter

package graph

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func TestReceiverResolutionUsesLocalEvidenceAndLeavesDynamicCallsUnresolved(t *testing.T) {
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
	if resolved != 6 || unresolved != 2 {
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

func TestInferredReceiverEdgesAgreeWithGoTypeChecker(t *testing.T) {
	source := `package p
type T struct{}
type U struct{}
type Alias = T
type Alias2 = Alias
type Pointer = *T
type I interface{Run()}
func (T) Run(){}
func (U) Run(){}
func Factory() T {return T{}}
func Known(t T,u U,a Alias,p Pointer){
 literal:=T{};pointer:=&T{};copyOf:=literal;var fromVar=Alias2{}
 first,second:=T{},U{}
 t.Run();u.Run();a.Run();p.Run()
 literal.Run();pointer.Run();copyOf.Run();fromVar.Run()
 first.Run();second.Run();T.Run(t);(Alias(T{})).Run()
 shadow:=T{};{shadow:=U{};shadow.Run()};shadow.Run()
}
func Dynamic(i I){i.Run();Factory().Run();p:=new(T);p.Run()}
func Generic[T interface{Run()}](value T){value.Run()}
`
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := new(types.Config).Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("invalid oracle fixture: %v", err)
	}
	expected := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Known" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Run" {
				return true
			}
			selection := info.Selections[sel]
			if selection == nil {
				t.Fatal("missing type selection")
			}
			method := selection.Obj().(*types.Func)
			typ := method.Type().(*types.Signature).Recv().Type()
			if pointer, ok := typ.(*types.Pointer); ok {
				typ = pointer.Elem()
			}
			named, ok := types.Unalias(typ).(*types.Named)
			if !ok {
				t.Fatalf("nonconcrete oracle: %v", typ)
			}
			pos := fset.Position(call.Pos())
			expected[fmt.Sprintf("%d:%d", pos.Line, pos.Column)] = named.Obj().Name()
			return true
		})
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT r.line,t.relation_id,coalesce(s.receiver,'') FROM relations r JOIN relation_targets t ON t.relation_id=r.id LEFT JOIN symbol_receivers s ON s.symbol_id=t.to_id WHERE r.from_symbol='Known' AND r.to_symbol='Run'`)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for rows.Next() {
		var line int
		var id, receiver string
		if err := rows.Scan(&line, &id, &receiver); err != nil {
			t.Fatal(err)
		}
		column := id[strings.LastIndex(id, ":")+1:]
		actual[fmt.Sprintf("%d:%s", line, column)] = receiver
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(actual, expected) || len(actual) != 14 {
		t.Fatalf("edges disagree with Go type checker: got %v want %v", actual, expected)
	}
	var dynamicResolved, dynamicObserved int
	if err := db.QueryRow(`SELECT count(*),count(t.to_id) FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.from_symbol IN ('Dynamic','Generic') AND r.to_symbol='Run'`).Scan(&dynamicObserved, &dynamicResolved); err != nil {
		t.Fatal(err)
	}
	if dynamicObserved != 4 || dynamicResolved != 0 {
		t.Fatalf("guessed dynamic/factory/builtin edges: %d observed %d resolved", dynamicObserved, dynamicResolved)
	}
	// Alias-only changes invalidate cached receiver evidence even with unchanged
	// callers. All aliases in this file must now resolve to the U method.
	changed := strings.Replace(source, "type Alias = T", "type Alias = U", 1)
	if err := os.WriteFile(filepath.Join(root, "p.go"), []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var aliases int
	if err := db.QueryRow(`SELECT count(*) FROM relations r JOIN relation_targets t ON t.relation_id=r.id JOIN symbol_receivers s ON s.symbol_id=t.to_id WHERE r.from_symbol='Known' AND r.to_symbol='Run' AND s.receiver='U'`).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 6 {
		t.Fatalf("stale alias receiver edges: %d", aliases)
	}
}

func TestReceiverEvidenceCyclesAndCrossFileShadowsRemainUnresolved(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"p.go": `package p
type T struct{}
type A = B
type B = A
var first = second
var second = first
func (T) Run(){}
func Unknown(a A){a.Run();first.Run();new(0).Run()}
`,
		"other.go": `package p
func new(int) T{return T{}}
`,
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var observed, resolved int
	if err := db.QueryRow(`SELECT count(*),count(t.to_id) FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.from_symbol='Unknown' AND r.to_symbol='Run'`).Scan(&observed, &resolved); err != nil {
		t.Fatal(err)
	}
	if observed != 3 || resolved != 0 {
		t.Fatalf("guessed cyclic/shadowed edges: %d observed %d resolved", observed, resolved)
	}
}
