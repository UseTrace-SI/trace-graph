//go:build treesitter

package graph

import (
	"fmt"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	tsc "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tscpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsja "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsrb "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tsrs "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
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

const analyzerVersion = "treesitter-v5"

func supportedFile(ext string) bool { _, ok := tsLangs[ext]; return ok }

// tsDeclarations are the declaration nodes of the pinned grammars. Each one
// must carry a name field, except C/C++ function_declarator, which is named
// by its declarator. A substring match on "function"/"method" used to promote
// call sites (Java method_invocation, C++ template_function, Rust
// generic_function), types (function_type) and parameters (Ruby
// method_parameters, arrow-function parameters and bodies) to declarations:
// same-name call sites on one line such as sb.append(a).append(b) or
// xs.map(x => x).filter(x => x) then collided and failed the whole index.
var tsDeclarations = map[string]bool{
	// Go
	"function_declaration": true, "method_declaration": true, "method_elem": true, "type_spec": true,
	// Python (and C/C++ function_definition, which has no name field)
	"function_definition": true, "class_definition": true,
	// JavaScript and TypeScript
	"generator_function_declaration": true, "method_definition": true, "class_declaration": true,
	"function_expression": true, "generator_function": true, "interface_declaration": true,
	"enum_declaration": true, "function_signature": true, "method_signature": true, "abstract_method_signature": true,
	// Rust
	"function_item": true, "function_signature_item": true, "struct_item": true, "enum_item": true, "trait_item": true,
	// C and C++
	"function_declarator": true, "preproc_function_def": true,
	// Ruby
	"method": true, "singleton_method": true,
}

// tsCallSites contain "function" or "method" in their kind but are
// expressions, so call extraction must look inside them.
var tsCallSites = map[string]bool{"method_invocation": true, "method_reference": true, "generic_function": true, "template_function": true, "template_method": true}

// tsNestedScope stops call attribution at nested function-like nodes, whose
// calls belong to them rather than to the enclosing declaration.
func tsNestedScope(kind string) bool {
	return (strings.Contains(kind, "function") || strings.Contains(kind, "method")) && !tsCallSites[kind]
}

// tsDeclarationName returns the declared name node, or nil for a node that is
// not a named declaration.
func tsDeclarationName(node *ts.Node) *ts.Node {
	if !tsDeclarations[node.Kind()] {
		return nil
	}
	if name := node.ChildByFieldName("name"); name != nil {
		return name
	}
	if node.Kind() != "function_declarator" {
		return nil
	}
	if declarator := node.ChildByFieldName("declarator"); declarator != nil && strings.HasSuffix(declarator.Kind(), "identifier") {
		return declarator
	}
	return nil
}

func analyzeFile(repo, rel, path string, src []byte, out *fileBatch) (int, error) {
	out.namespace = rel
	n := 0
	lang := tsLangs[strings.ToLower(filepathExt(rel))]
	parser := ts.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return 0, err
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return 0, fmt.Errorf("parsing %s", rel)
	}
	defer tree.Close()
	root := tree.RootNode()
	// walk
	var walk func(node *ts.Node)
	walk = func(node *ts.Node) {
		if node == nil {
			return
		}
		kind := node.Kind()
		if nameNode := tsDeclarationName(node); nameNode != nil {
			name := nameNode.Utf8Text(src)
			line := int(node.StartPosition().Row) + 1
			symKind := "func"
			if strings.Contains(kind, "class") || strings.Contains(kind, "struct") || strings.Contains(kind, "enum") ||
				strings.Contains(kind, "trait") || strings.Contains(kind, "interface") || strings.Contains(kind, "type") {
				symKind = "type"
			}
			id := rel + ":" + name + ":" + itoa(line)
			out.symbol(id, name, symKind, line, kind+" "+name)
			out.content(id, src[node.StartByte():node.EndByte()], int(node.EndPosition().Row)+1)
			n++
			// calls within: find call_expression children
			var findCalls func(n *ts.Node)
			findCalls = func(n *ts.Node) {
				if n == nil {
					return
				}
				if n.Id() != node.Id() && tsNestedScope(n.Kind()) {
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
						out.call(id, name, callee, int(n.StartPosition().Row)+1, int(n.StartPosition().Column)+1, "unresolved")
					}
				}
				for i := uint(0); i < n.ChildCount(); i++ {
					findCalls(n.Child(i))
				}
			}
			findCalls(node)
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			walk(node.Child(i))
		}
	}
	walk(root)
	// Imports and HTTP routes come from the shared pure-Go extractor. Without
	// lexical containers, Tree-sitter symbols are never module-level targets,
	// so these resolve to module files (and decorated Python handlers) only.
	if lang := relationLanguage(rel); lang != "" {
		writeLexRelations(lang, repo, rel, newLexSource(lang, string(src)), out, func(int) (string, string) { return "", "" }, nil)
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
