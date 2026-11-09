//go:build !treesitter

package graph

import "go/ast"

// Resolution follows only syntax evidence in the current file. Each traversal
// is bounded, including malformed alias/initializer cycles. Cross-file aliases,
// factory returns, embedded promotion and dynamic dispatch stay unresolved.
const receiverEvidenceDepth = 32

func declaredTypeName(expr ast.Expr) string { return localTypeName(expr, 0) }

func localTypeName(expr ast.Expr, depth int) string {
	if depth >= receiverEvidenceDepth {
		return ""
	}
	switch x := expr.(type) {
	case *ast.StarExpr:
		return localTypeName(x.X, depth+1)
	case *ast.ParenExpr:
		return localTypeName(x.X, depth+1)
	case *ast.IndexExpr:
		return localTypeName(x.X, depth+1)
	case *ast.IndexListExpr:
		return localTypeName(x.X, depth+1)
	case *ast.Ident:
		if x.Obj != nil {
			spec, ok := x.Obj.Decl.(*ast.TypeSpec)
			if !ok {
				return ""
			}
			if spec.Assign.IsValid() {
				return localTypeName(spec.Type, depth+1)
			}
			return x.Name
		}
		// Another file may declare this receiver. Resolution still requires one
		// exact method identity in the same package namespace.
		return x.Name
	}
	return ""
}

func receiverType(expr ast.Expr) string { return localReceiverType(expr, 0) }

func localReceiverType(expr ast.Expr, depth int) string {
	if depth >= receiverEvidenceDepth {
		return ""
	}
	switch x := expr.(type) {
	case *ast.ParenExpr:
		return localReceiverType(x.X, depth+1)
	case *ast.UnaryExpr:
		if x.Op.String() == "&" {
			return localReceiverType(x.X, depth+1)
		}
	case *ast.CompositeLit:
		return localTypeName(x.Type, depth+1)
	case *ast.CallExpr:
		// Only conversions whose identifier is a locally declared type supply
		// evidence. Even new(T) may be a package-level function in another file.
		if identifier := localTypeIdentifier(x.Fun); identifier != nil && identifier.Obj != nil {
			if _, ok := identifier.Obj.Decl.(*ast.TypeSpec); ok {
				return localTypeName(x.Fun, depth+1)
			}
		}
	case *ast.Ident:
		if x.Obj == nil {
			return ""
		}
		switch decl := x.Obj.Decl.(type) {
		case *ast.Field:
			return localTypeName(decl.Type, depth+1)
		case *ast.ValueSpec:
			if decl.Type != nil {
				return localTypeName(decl.Type, depth+1)
			}
			if len(decl.Names) == len(decl.Values) {
				for index, name := range decl.Names {
					if name.Obj == x.Obj {
						return localReceiverType(decl.Values[index], depth+1)
					}
				}
			}
		case *ast.AssignStmt:
			if len(decl.Lhs) == len(decl.Rhs) {
				for index, left := range decl.Lhs {
					if name, ok := left.(*ast.Ident); ok && name.Obj == x.Obj {
						return localReceiverType(decl.Rhs[index], depth+1)
					}
				}
			}
		case *ast.TypeSpec:
			return localTypeName(x, depth+1)
		}
	}
	return ""
}

func localTypeIdentifier(expr ast.Expr) *ast.Ident {
	for depth := 0; depth < receiverEvidenceDepth; depth++ {
		switch x := expr.(type) {
		case *ast.Ident:
			return x
		case *ast.ParenExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		case *ast.IndexListExpr:
			expr = x.X
		default:
			return nil
		}
	}
	return nil
}
