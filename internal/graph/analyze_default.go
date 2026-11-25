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

// langExts maps supported source extensions to languages. Configuration
// formats such as YAML are not sources: code-shaped regular expressions
// only found false declarations in them.
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
	".cxx":    "cpp",
	".h":      "c",
	".hh":     "cpp",
	".hpp":    "cpp",
	".hxx":    "cpp",
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

const analyzerVersion = "inventory-v8"

func supportedFile(ext string) bool { _, ok := langExts[ext]; return ok }

func analyzeFile(repo, rel, path string, src []byte, out *fileBatch) (int, error) {
	if lang := langExts[strings.ToLower(filepathExt(rel))]; lang != "go" {
		if lexDeclarationLanguage(lang) {
			return analyzeLexicalFile(lang, repo, rel, string(src), out)
		}
		return analyzeRegexFile(repo, rel, string(src), out)
	}
	out.namespace = pathpkg.Dir(rel)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return analyzeRegexFile(repo, rel, string(src), out)
	}
	out.namespace = pathpkg.Dir(rel) + "|" + file.Name.Name
	// Symbols and calls live at physical positions in this file. //line
	// directives in generated code remap lines (and zero columns), which
	// would move symbols and merge distinct call sites into one ID.
	position := func(pos token.Pos) token.Position { return fset.PositionFor(pos, false) }
	packageLevel := filePackageTypes(file)
	imports := map[string]string{}
	module := ""
	if out.moduleOverride != nil {
		module = *out.moduleOverride
	} else {
		module = moduleName(repo)
	}
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
			// The blank identifier declares nothing; `var _, _ = f()` used to
			// yield two identical identities and fail the whole index.
			if d.Name.Name == "_" {
				continue
			}
			line := position(d.Pos()).Line
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			id := goSymbolID(rel, d.Name.Name, line)
			out.symbol(id, d.Name.Name, kind, line, goSignature(fset, d))
			out.content(id, src[position(d.Pos()).Offset:position(d.End()).Offset], position(d.End()).Line)
			if d.Recv != nil && len(d.Recv.List) == 1 {
				if receiver := declaredTypeName(packageLevel, d.Recv.List[0].Type); receiver != "" {
					out.receiverOf(id, receiver)
				}
			}
			n++
			if d.Body == nil {
				continue
			}
			hintFor := func(expr ast.Expr) string {
				hint := "unresolved"
				switch fn := expr.(type) {
				case *ast.Ident:
					if fn.Obj == nil || fn.Obj.Kind == ast.Fun {
						hint = "identifier"
					}
				case *ast.SelectorExpr:
					if alias, ok := fn.X.(*ast.Ident); ok && alias.Obj == nil {
						if dir, ok := imports[alias.Name]; ok {
							hint = "package:" + dir
						}
					}
					if hint == "unresolved" {
						if receiver := receiverType(packageLevel, fn.X); receiver != "" {
							hint = "receiver:" + receiver
						}
					}
				}
				return hint
			}
			for _, site := range goCallSites(d.Body) {
				at := position(site.call.Pos())
				out.call(id, d.Name.Name, site.callee, at.Line, at.Column, hintFor(site.fun))
				if method, path, handler, ok := goRoute(site); ok {
					// The handler reference resolves like a call of it.
					out.relation(goRouteRelationID(id, at.Line, at.Column), id, method+" "+path, goHandlerName(handler), "route", at.Line, hintFor(handler))
				}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name != "_" {
					line := position(ts.Pos()).Line
					id := goSymbolID(rel, ts.Name.Name, line)
					out.symbol(id, ts.Name.Name, "type", line, "type "+ts.Name.Name)
					out.content(id, src[position(ts.Pos()).Offset:position(ts.End()).Offset], position(ts.End()).Line)
					n++
				}
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, nm := range vs.Names {
						if nm.Name == "_" {
							continue
						}
						line := position(nm.Pos()).Line
						id := goSymbolID(rel, nm.Name, line)
						out.symbol(id, nm.Name, "var", line, "var "+nm.Name)
						out.content(id, src[position(vs.Pos()).Offset:position(vs.End()).Offset], position(vs.End()).Line)
						n++
					}
				}
			}
		}
	}
	return n, nil
}

func analyzeRegexFile(repo, rel, src string, out *fileBatch) (int, error) {
	out.namespace = pathpkg.Dir(rel)
	// Classification and the source hash use the masked line. A trailing
	// "// class marker" comment must not turn an arrow function into a type.
	lines := strings.Split(maskInventory(langExts[strings.ToLower(filepathExt(rel))], src), "\n")
	n := 0
	for i, line := range lines {
		for _, pat := range symbolPatterns {
			m := pat.FindStringSubmatch(line)
			if len(m) >= 2 {
				name := m[1]
				visible := strings.TrimSpace(line)
				kind := "symbol"
				if strings.Contains(visible, "func ") || strings.Contains(visible, "def ") || strings.Contains(visible, "fn ") || strings.Contains(visible, "function ") || strings.Contains(visible, "fun ") {
					kind = "func"
				} else if strings.Contains(visible, "class ") || strings.Contains(visible, "interface ") || strings.Contains(visible, "enum ") || strings.Contains(visible, "type ") || strings.Contains(visible, "struct ") || strings.Contains(visible, "trait ") || strings.Contains(visible, "object ") || strings.Contains(visible, "protocol ") || strings.Contains(visible, "module ") || strings.Contains(visible, "defmodule ") || strings.Contains(visible, "defn ") {
					kind = "type"
				}
				id := rel + ":" + name + ":" + itoa(i+1)
				out.symbol(id, name, kind, i+1, visible)
				out.content(id, []byte(visible), i+1)
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
