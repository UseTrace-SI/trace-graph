//go:build !treesitter

package graph

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"
)

var langExts = map[string]string{
	".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".mts": "typescript", ".cts": "typescript",
	".go":     "go",
	".py":     "python",
	".js":     "javascript",
	".ts":     "typescript",
	".tsx":    "typescript",
	".rs":     "rust",
	".java":   "java",
	".c":      "c",
	".cpp":    "cpp",
	".cc":     "cpp",
	".rb":     "ruby",
	".cs":     "csharp",
	".kt":     "kotlin",
	".swift":  "swift",
	".scala":  "scala",
	".dart":   "dart",
	".php":    "php",
	".lua":    "lua",
	".r":      "r",
	".sql":    "sql",
	".sh":     "bash",
	".bash":   "bash",
	".zsh":    "zsh",
	".hs":     "haskell",
	".ex":     "elixir",
	".exs":    "elixir",
	".erl":    "erlang",
	".clj":    "clojure",
	".cljs":   "clojurescript",
	".cue":    "cue",
	".groovy": "groovy",
	".hcl":    "hcl",
	".tf":     "hcl",
	".jl":     "julia",
	".ml":     "ocaml",
	".m":      "objectivec",
	".pl":     "perl",
	".proto":  "protobuf",
	".yaml":   "yaml",
	".yml":    "yaml",
	".zig":    "zig",
}

var symbolPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z_$][A-Za-z0-9_$]*)\s*(?::[^=]+)?=>`),
	regexp.MustCompile(`(?m)^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?class\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:class|interface|enum|struct|trait|object|protocol)\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*type\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:fun|val|var)\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*module\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*defmodule\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*defn\s+([A-Za-z_][A-Za-z0-9_]*)`),
}

const analyzerVersion = "inventory-v5"

func supportedFile(ext string) bool { _, ok := langExts[ext]; return ok }

func analyzeFile(repo, rel, path string, src []byte, db *indexWriter) (int, error) {
	if langExts[strings.ToLower(filepathExt(rel))] != "go" {
		return analyzeRegexFile(repo, rel, string(src), db)
	}
	db.namespace = pathpkg.Dir(rel)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return analyzeRegexFile(repo, rel, string(src), db)
	}
	db.namespace = pathpkg.Dir(rel) + "|" + file.Name.Name
	imports := map[string]string{}
	module := moduleName(repo)
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		alias := pathpkg.Base(importPath)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		if module != "" && strings.HasPrefix(importPath, module+"/") {
			imports[alias] = strings.TrimPrefix(importPath, module+"/")
		}
	}
	n := 0
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			line := fset.Position(d.Pos()).Line
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			id := rel + ":" + d.Name.Name + ":" + itoa(line)
			db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
				id, repo, rel, d.Name.Name, kind, line, goSignature(fset, d))
			db.content(repo, id, src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset], fset.Position(d.End()).Line)
			if d.Recv != nil && len(d.Recv.List) == 1 {
				if receiver := declaredTypeName(d.Recv.List[0].Type); receiver != "" && db.err == nil {
					_, db.err = db.tx.Exec("INSERT INTO symbol_receivers(symbol_id,receiver) VALUES(?,?) ON CONFLICT(symbol_id) DO UPDATE SET receiver=excluded.receiver", scopedID(repo, id), receiver)
				}
			}
			n++
			if d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(node ast.Node) bool {
				// A closure's calls belong to the closure, not to the enclosing function.
				if _, ok := node.(*ast.FuncLit); ok {
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					hint := "unresolved"
					if fn.Obj == nil || fn.Obj.Kind == ast.Fun {
						hint = "identifier"
					}
					db.call(repo, rel, id, d.Name.Name, fn.Name, fset.Position(call.Pos()).Line, fset.Position(call.Pos()).Column, hint)
				case *ast.SelectorExpr:
					hint := "unresolved"
					if alias, ok := fn.X.(*ast.Ident); ok && alias.Obj == nil {
						if dir, ok := imports[alias.Name]; ok {
							hint = "package:" + dir
						}
					}
					if hint == "unresolved" {
						if receiver := receiverType(fn.X); receiver != "" {
							hint = "receiver:" + receiver
						}
					}
					db.call(repo, rel, id, d.Name.Name, fn.Sel.Name, fset.Position(call.Pos()).Line, fset.Position(call.Pos()).Column, hint)
				}
				return true
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					line := fset.Position(ts.Pos()).Line
					id := rel + ":" + ts.Name.Name + ":" + itoa(line)
					db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
						id, repo, rel, ts.Name.Name, "type", line, "type "+ts.Name.Name)
					db.content(repo, id, src[fset.Position(ts.Pos()).Offset:fset.Position(ts.End()).Offset], fset.Position(ts.End()).Line)
					n++
				}
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, nm := range vs.Names {
						line := fset.Position(nm.Pos()).Line
						id := rel + ":" + nm.Name + ":" + itoa(line)
						db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
							id, repo, rel, nm.Name, "var", line, "var "+nm.Name)
						db.content(repo, id, src[fset.Position(vs.Pos()).Offset:fset.Position(vs.End()).Offset], fset.Position(vs.End()).Line)
						n++
					}
				}
			}
		}
	}
	return n, nil
}

func analyzeRegexFile(repo, rel, src string, db *indexWriter) (int, error) {
	db.namespace = pathpkg.Dir(rel)
	lines := strings.Split(maskInventory(src), "\n")
	original := strings.Split(src, "\n")
	n := 0
	for i, line := range lines {
		for _, pat := range symbolPatterns {
			m := pat.FindStringSubmatch(line)
			if len(m) >= 2 {
				name := m[1]
				kind := "symbol"
				if strings.Contains(line, "func ") || strings.Contains(line, "def ") || strings.Contains(line, "fn ") || strings.Contains(line, "function ") || strings.Contains(line, "fun ") {
					kind = "func"
				} else if strings.Contains(line, "class ") || strings.Contains(line, "interface ") || strings.Contains(line, "enum ") || strings.Contains(line, "type ") || strings.Contains(line, "struct ") || strings.Contains(line, "trait ") || strings.Contains(line, "object ") || strings.Contains(line, "protocol ") || strings.Contains(line, "module ") || strings.Contains(line, "defmodule ") || strings.Contains(line, "defn ") {
					kind = "type"
				}
				id := rel + ":" + name + ":" + itoa(i+1)
				db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
					id, repo, rel, name, kind, i+1, strings.TrimSpace(original[i]))
				db.content(repo, id, []byte(original[i]), i+1)
				n++
				break
			}
		}
	}
	return n, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func goSignature(fset *token.FileSet, d *ast.FuncDecl) string {
	copy := *d
	copy.Body = nil
	copy.Doc = nil
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, &copy); err != nil {
		return "func " + d.Name.Name
	}
	return buf.String()
}
