//go:build !treesitter

package graph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Calls whose function is parenthesized or explicitly instantiated used to
// be skipped entirely, so F[int](x) was neither observed nor resolvable.
func TestGoCallSitesObserveInstantiatedAndParenthesizedCallees(t *testing.T) {
	src := `package p
func F[T any](T) {}
func G[K comparable, V any](K, V) {}
type T struct{}
func (T) M() {}
func Use(t T, fns map[string]func()) {
	F[int](1)
	G[string, int]("a", 1)
	(F[bool])(true)
	(t.M)()
	t.M()
	fns["x"]()
	func() { Hidden() }()
	F(Inner())
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Use" {
			for _, site := range goCallSites(fn.Body) {
				at := fset.Position(site.call.Pos())
				got = append(got, site.callee+"@"+itoa(at.Line))
			}
		}
	}
	want := "F@7 G@8 F@9 M@10 M@11 fns@12 F@14 Inner@14"
	if strings.Join(got, " ") != want {
		t.Fatalf("call sites %v, want %s", got, want)
	}
}
