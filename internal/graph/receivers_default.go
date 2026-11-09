//go:build !treesitter

package graph

import "go/ast"

// Only explicit local type declarations supply evidence for a receiver edge.
// Aliases, embedded-field promotion, type parameters and inferred return types
// deliberately remain unresolved; this is syntax analysis, not a type checker.
func declaredTypeName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.StarExpr:
		return declaredTypeName(x.X)
	case *ast.ParenExpr:
		return declaredTypeName(x.X)
	case *ast.IndexExpr:
		return declaredTypeName(x.X)
	case *ast.IndexListExpr:
		return declaredTypeName(x.X)
	case *ast.Ident:
		if x.Obj != nil {
			if spec, ok := x.Obj.Decl.(*ast.TypeSpec); ok && !spec.Assign.IsValid() {
				return x.Name
			}
			return ""
		}
		// A declaration in another file of the same package is resolved later
		// against the namespace's exact receiver identities, never by name alone.
		return x.Name
	}
	return ""
}

func receiverType(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.ParenExpr:
		return receiverType(x.X)
	case *ast.UnaryExpr:
		if x.Op.String() == "&" {
			return receiverType(x.X)
		}
	case *ast.CompositeLit:
		return declaredTypeName(x.Type)
	case *ast.Ident:
		if x.Obj == nil {
			return ""
		}
		switch decl := x.Obj.Decl.(type) {
		case *ast.Field:
			return declaredTypeName(decl.Type)
		case *ast.ValueSpec:
			return declaredTypeName(decl.Type)
		case *ast.TypeSpec:
			return declaredTypeName(x)
		}
	}
	return ""
}
