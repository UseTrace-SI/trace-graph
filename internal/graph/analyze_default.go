//go:build !treesitter

package graph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
)

var langExts = map[string]string{
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
	regexp.MustCompile(`(?m)^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:export\s+)?class\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*def\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:class|interface|enum|struct|trait|object|protocol)\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*type\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*(?:fun|val|var)\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*module\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*defmodule\s+([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`(?m)^\s*defn\s+([A-Za-z_][A-Za-z0-9_]*)`),
}

const analyzerVersion = "inventory-v2"

func supportedFile(ext string) bool { _, ok := langExts[ext]; return ok }

func analyzeFile(repo, rel, path string, src []byte, db *indexWriter) (int, error) {
	if langExts[strings.ToLower(filepathExt(rel))] != "go" {
		return analyzeRegexFile(repo, rel, string(src), db)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return analyzeRegexFile(repo, rel, string(src), db)
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
				id, repo, rel, d.Name.Name, kind, line, "func "+d.Name.Name)
			n++
			if d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok {
						rid := rel + ":" + d.Name.Name + "->" + ident.Name + ":" + itoa(fset.Position(call.Pos()).Line)
						db.Exec("INSERT OR REPLACE INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?)",
							rid, repo, d.Name.Name, ident.Name, "call", rel, fset.Position(call.Pos()).Line)
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						rid := rel + ":" + d.Name.Name + "->" + sel.Sel.Name + ":" + itoa(fset.Position(call.Pos()).Line)
						db.Exec("INSERT OR REPLACE INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?)",
							rid, repo, d.Name.Name, sel.Sel.Name, "call", rel, fset.Position(call.Pos()).Line)
					}
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
					n++
				}
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, nm := range vs.Names {
						line := fset.Position(nm.Pos()).Line
						id := rel + ":" + nm.Name + ":" + itoa(line)
						db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
							id, repo, rel, nm.Name, "var", line, "var "+nm.Name)
						n++
					}
				}
			}
		}
	}
	return n, nil
}

func analyzeRegexFile(repo, rel, src string, db *indexWriter) (int, error) {
	lines := strings.Split(src, "\n")
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
					id, repo, rel, name, kind, i+1, strings.TrimSpace(line))
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
