//go:build !treesitter

package graph

import "go/ast"

// goCallSite is one call observed in a function body. The per-file analyzer
// stores it as a relation; the package-wide type pass enumerates the same
// sites from the same bytes and derives the identical relation ID to attach a
// type-checked target.
type goCallSite struct {
	call   *ast.CallExpr
	fun    ast.Expr // *ast.Ident or *ast.SelectorExpr naming the callee
	callee string
}

// goCallee strips parentheses and explicit instantiation (F[int],
// pkg.F[K, V]) from a call's function expression. Calls of other
// expressions (closures, call results) have no static name.
func goCallee(expr ast.Expr) (ast.Expr, string) {
	for depth := 0; depth < receiverEvidenceDepth; depth++ {
		switch x := expr.(type) {
		case *ast.ParenExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		case *ast.IndexListExpr:
			expr = x.X
		case *ast.Ident:
			return x, x.Name
		case *ast.SelectorExpr:
			return x, x.Sel.Name
		default:
			return nil, ""
		}
	}
	return nil, ""
}

// goCallSites lists the calls in a function body in source order. A
// closure's calls belong to the closure, not to the enclosing function.
func goCallSites(body *ast.BlockStmt) []goCallSite {
	var sites []goCallSite
	ast.Inspect(body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fun, name := goCallee(call.Fun); fun != nil {
			sites = append(sites, goCallSite{call: call, fun: fun, callee: name})
		}
		return true
	})
	return sites
}
