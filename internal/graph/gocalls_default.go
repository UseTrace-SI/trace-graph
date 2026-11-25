//go:build !treesitter

package graph

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

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

var goRouteVerbs = map[string]string{
	"HandleFunc": "*", "Handle": "*", "Any": "*",
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
	"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE", "PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
}

var goHTTPMethods = wordSet("GET HEAD POST PUT PATCH DELETE CONNECT OPTIONS TRACE")

// goRoute recognizes an HTTP route registration: net/http and gorilla
// HandleFunc/Handle (including Go 1.22 "GET /path" patterns) and chi, gin
// and echo verb methods, called on a mux, router or package with a literal
// path and a handler-shaped last argument.
func goRoute(site goCallSite) (string, string, ast.Expr, bool) {
	verb, known := goRouteVerbs[site.callee]
	if _, selector := site.fun.(*ast.SelectorExpr); !known || !selector || len(site.call.Args) < 2 {
		return "", "", nil, false
	}
	literal, ok := site.call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", "", nil, false
	}
	pattern, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", "", nil, false
	}
	if verb == "*" {
		if method, rest, found := strings.Cut(pattern, " "); found && goHTTPMethods[method] {
			verb, pattern = method, strings.TrimSpace(rest)
		}
	}
	if !strings.HasPrefix(pattern, "/") {
		return "", "", nil, false
	}
	handler := site.call.Args[len(site.call.Args)-1]
	for depth := 0; depth < receiverEvidenceDepth; depth++ {
		if paren, ok := handler.(*ast.ParenExpr); ok {
			handler = paren.X
		}
	}
	switch handler.(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.FuncLit, *ast.CallExpr, *ast.UnaryExpr, *ast.CompositeLit:
		return verb, pattern, handler, true
	}
	return "", "", nil, false
}

// goHandlerName names a route's handler; inline handlers have no name.
func goHandlerName(handler ast.Expr) string {
	switch h := handler.(type) {
	case *ast.Ident:
		return h.Name
	case *ast.SelectorExpr:
		return h.Sel.Name
	}
	return "(inline)"
}

func goRouteRelationID(fromID string, line, column int) string {
	return fromID + "->route:" + strconv.Itoa(line) + ":" + strconv.Itoa(column)
}
