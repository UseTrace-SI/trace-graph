//go:build treesitter

package graph

import (
	"os"
	"path/filepath"
	"strings"

	"database/sql"

	ts "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	tsrs "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsja "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsc "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tscpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tsrb "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

var tsLangs = map[string]*ts.Language{
	".go":   ts.NewLanguage(tsgo.Language()),
	".py":   ts.NewLanguage(tspy.Language()),
	".js":   ts.NewLanguage(tsjs.Language()),
	".ts":   ts.NewLanguage(tsts.LanguageTypescript()),
	".tsx":  ts.NewLanguage(tsts.LanguageTSX()),
	".rs":   ts.NewLanguage(tsrs.Language()),
	".java": ts.NewLanguage(tsja.Language()),
	".c":    ts.NewLanguage(tsc.Language()),
	".cpp":  ts.NewLanguage(tscpp.Language()),
	".cc":   ts.NewLanguage(tscpp.Language()),
	".rb":   ts.NewLanguage(tsrb.Language()),
}

func AnalyzeRepo(repo string, db *sql.DB) (int, error) {
	n := 0
	err := filepath.Walk(repo, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		lang, ok := tsLangs[ext]
		if !ok {
			// fallback: skip (default build handles others)
			return nil
		}
		rel, _ := filepath.Rel(repo, path)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		parser := ts.NewParser()
		defer parser.Close()
		parser.SetLanguage(lang)
		tree := parser.Parse(src, nil)
		defer tree.Close()
		root := tree.RootNode()
		// walk
		var walk func(node *ts.Node)
		walk = func(node *ts.Node) {
			if node == nil {
				return
			}
			kind := node.Kind()
			isSym := kind == "function_declaration" || kind == "function_definition" || kind == "method_declaration" ||
				kind == "class_definition" || kind == "type_spec" || kind == "type_declaration" ||
				kind == "function_item" || kind == "struct_item" || kind == "enum_item" ||
				kind == "trait_item" || kind == "impl_item" || kind == "interface_declaration" ||
				kind == "class_declaration" || kind == "enum_declaration" || kind == "method_definition" ||
				strings.Contains(kind, "function") || strings.Contains(kind, "method")
			if isSym {
				nameNode := node.ChildByFieldName("name")
				if nameNode == nil && node.ChildCount() > 0 {
					for i := uint(0); i < node.ChildCount(); i++ {
						c := node.Child(i)
						if c != nil && !c.IsNamed() {
							continue
						}
						if c != nil && (c.Kind() == "identifier" || c.Kind() == "type_identifier" || strings.HasSuffix(c.Kind(), "identifier")) {
							nameNode = c
							break
						}
					}
				}
				if nameNode != nil {
					name := nameNode.Utf8Text(src)
					line := int(node.StartPosition().Row) + 1
					symKind := "func"
					if strings.Contains(kind, "class") || strings.Contains(kind, "struct") || strings.Contains(kind, "enum") ||
						strings.Contains(kind, "trait") || strings.Contains(kind, "interface") || strings.Contains(kind, "type") {
						symKind = "type"
					}
					id := rel + ":" + name + ":" + itoa(line)
					db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
						id, repo, rel, name, symKind, line, kind+" "+name)
					n++
					// calls within: find call_expression children
					var findCalls func(n *ts.Node)
					findCalls = func(n *ts.Node) {
						if n == nil {
							return
						}
						if n.Kind() == "call_expression" || n.Kind() == "call" || n.Kind() == "method_invocation" {
							fn := n.ChildByFieldName("function")
							if fn == nil {
								fn = n.ChildByFieldName("name")
							}
							if fn != nil {
								callee := fn.Utf8Text(src)
								// take last identifier
								if idx := strings.LastIndex(callee, "."); idx >= 0 {
									callee = callee[idx+1:]
								}
								rid := rel + ":" + name + "->" + callee + ":" + itoa(int(n.StartPosition().Row)+1)
								db.Exec("INSERT OR REPLACE INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?)",
									rid, repo, name, callee, "call", rel, int(n.StartPosition().Row)+1)
							}
						}
						for i := uint(0); i < n.ChildCount(); i++ {
							findCalls(n.Child(i))
						}
					}
					findCalls(node)
				}
			}
			for i := uint(0); i < node.ChildCount(); i++ {
				walk(node.Child(i))
			}
		}
		walk(root)
		return nil
	})
	return n, err
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
