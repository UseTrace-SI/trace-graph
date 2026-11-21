package graph

import (
	pathpkg "path"
	"regexp"
	"strings"
)

// Import and HTTP-route extraction for TypeScript/JavaScript, Python, Rust
// and Java. It reads structure from masked source (comments and string
// contents blanked, offsets preserved) so commented-out or quoted code never
// becomes an edge, and reads literal values (module specifiers, route paths)
// from the original bytes at the same offsets. It is pure Go and shared by
// both builds.

// lexLocalContainer marks declarations nested inside a function (and symbols
// of analyzers without containers): they are neither module-level nor
// members, so they are never resolution targets.
const lexLocalContainer = "<local>"

func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[i+2:]
	}
	return path
}

func splitRustPath(path string) (string, string) {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[:i], path[i+2:]
	}
	return "", path
}

// relationLanguage returns the language whose imports and routes are
// extracted for a file extension, or "".
func relationLanguage(rel string) string {
	switch strings.ToLower(pathpkg.Ext(rel)) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	}
	return ""
}

const (
	lexStatementLimit  = 8192 // bytes scanned for one import statement or call
	lexRouteLimit      = 4096 // route-path and specifier literal length
	lexDecoratorWindow = 64   // lines between a route decorator and its def
)

type lexSource struct {
	orig, mask string
	lines      []int       // byte offset of each line start
	literals   map[int]int // string literal start offset -> end offset
	braces     map[int]int // '{' offset -> offset just past its '}'
	code       string      // mask with string literals shown as x
}

func newLexSource(lang, src string) *lexSource {
	mask, literals := maskSpans(lang, src, false, true)
	s := &lexSource{orig: src, mask: mask, lines: []int{0}, literals: literals}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			s.lines = append(s.lines, i+1)
		}
	}
	// Brace bodies are matched once per file, so finding each declaration's
	// body is constant time however deeply bodies nest.
	s.braces = map[int]int{}
	var open []int
	for i := 0; i < len(mask); i++ {
		switch mask[i] {
		case '{':
			open = append(open, i)
		case '}':
			if len(open) > 0 {
				s.braces[open[len(open)-1]] = i + 1
				open = open[:len(open)-1]
			}
		}
	}
	code := []byte(mask)
	for start, end := range literals {
		for i := start; i < end; i++ {
			if code[i] != '\n' && code[i] != '\r' {
				code[i] = 'x'
			}
		}
	}
	s.code = string(code)
	return s
}

// closeBrace returns the offset just past the brace closing the one at
// open, or -1.
func (s *lexSource) closeBrace(open int) int {
	if end, ok := s.braces[open]; ok {
		return end
	}
	return -1
}

// codeLine returns a 1-based line with comments masked and string literals
// shown as x.
func (s *lexSource) codeLine(line int) string {
	if line < 1 || line > len(s.lines) {
		return ""
	}
	start, end := s.lines[line-1], len(s.code)
	if line < len(s.lines) {
		end = s.lines[line] - 1
	}
	return s.code[start:end]
}

// position returns the 1-based line and column of a byte offset.
func (s *lexSource) position(offset int) (int, int) {
	lo, hi := 0, len(s.lines)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if s.lines[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1, offset - s.lines[lo] + 1
}

// skipSpace skips whitespace and comments (both masked to spaces) and stops
// at a string literal's opening quote. A quote inside a comment is not a
// literal: treating it as one read a "path" from the comment and, past the
// call's closing parenthesis, panicked.
func (s *lexSource) skipSpace(i int) int {
	for i < len(s.mask) && (s.mask[i] == ' ' || s.mask[i] == '\t' || s.mask[i] == '\r' || s.mask[i] == '\n') {
		if _, ok := s.literals[i]; ok {
			return i
		}
		i++
	}
	return i
}

// stringAt reads the string literal starting at i (after whitespace and
// comments). Prefixed Python literals such as r"/x" are accepted; f-strings,
// triple-quoted strings and interpolated template literals are rejected.
func (s *lexSource) stringAt(i int) (string, int, bool) {
	i = s.skipSpace(i)
	if i+1 < len(s.orig) && strings.ContainsRune("rRbBuU", rune(s.orig[i])) {
		if _, ok := s.literals[i+1]; ok {
			i++
		}
	}
	end, ok := s.literals[i]
	if !ok || end-i > lexRouteLimit || end-i < 2 {
		return "", i, false
	}
	quote := s.orig[i]
	if end-i >= 6 && s.orig[i+1] == quote && s.orig[i+2] == quote {
		return "", i, false
	}
	var value strings.Builder
	for j := i + 1; j < end-1; j++ {
		c := s.orig[j]
		switch {
		case c == '\\' && j+1 < end-1:
			value.WriteByte(s.orig[j+1])
			j++
		case c == '$' && quote == '`' && j+1 < end-1 && s.orig[j+1] == '{':
			return "", i, false
		default:
			value.WriteByte(c)
		}
	}
	return value.String(), end, true
}

func lexIdentByte(c byte, dollar bool) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (dollar && c == '$')
}

// identAt reads an identifier from the masked text at i (after whitespace).
func (s *lexSource) identAt(i int) (string, int) {
	i = s.skipSpace(i)
	j := i
	for j < len(s.mask) && lexIdentByte(s.mask[j], true) {
		j++
	}
	if j == i || (s.mask[i] >= '0' && s.mask[i] <= '9') {
		return "", i
	}
	return s.mask[i:j], j
}

func (s *lexSource) punctAt(i int, p string) (int, bool) {
	i = s.skipSpace(i)
	if strings.HasPrefix(s.mask[i:], p) {
		return i + len(p), true
	}
	return i, false
}

// matching returns the offset just past the bracket closing the one at open.
func (s *lexSource) matching(open int, limit int) int {
	pairs := map[byte]byte{'(': ')', '[': ']', '{': '}'}
	var stack []byte
	for i := open; i < len(s.mask) && i-open <= limit; i++ {
		c := s.mask[i]
		if closer, ok := pairs[c]; ok {
			stack = append(stack, closer)
		} else if len(stack) > 0 && c == stack[len(stack)-1] {
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1
			}
		} else if c == ')' || c == ']' || c == '}' {
			return -1
		}
	}
	return -1
}

// lexImport is one binding introduced by an import statement.
type lexImport struct {
	offset    int
	specifier string // module as written: './x', 'a.b', 'crate::a', 'com.x'
	imported  string // name taken from the module, "" for the module itself
	local     string // local binding, "" when none
	kind      string // named, default, namespace, module, reexport or mod
}

// lexRoute is one HTTP route registration.
type lexRoute struct {
	offset       int
	method, path string
	handler      string // "" for an inline handler
	qualifier    string // object holding the handler, e.g. controllers
	decorated    int    // Python: line of the decorated function, 0 otherwise
}

var (
	jsStatementStart = regexp.MustCompile(`(?m)^[ \t]*(import|export)\b`)
	jsRequire        = regexp.MustCompile(`(?m)^[ \t]*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*|\{[^}]*\})\s*=\s*require\s*\(`)
	jsDynamicImport  = regexp.MustCompile(`\bimport\s*\(`)
	pyImport         = regexp.MustCompile(`(?m)^[ \t]*import[ \t]+([A-Za-z_][\w.]*(?:[ \t]+as[ \t]+[A-Za-z_]\w*)?(?:[ \t]*,[ \t]*[A-Za-z_][\w.]*(?:[ \t]+as[ \t]+[A-Za-z_]\w*)?)*)`)
	pyFromImport     = regexp.MustCompile(`(?m)^[ \t]*from[ \t]+(\.*)([A-Za-z_][\w.]*)?[ \t]+import[ \t]+`)
	rustUse          = regexp.MustCompile(`(?m)^[ \t]*(?:pub(?:\([^)]*\))?[ \t]+)?use[ \t]+`)
	rustMod          = regexp.MustCompile(`(?m)^[ \t]*(?:pub(?:\([^)]*\))?[ \t]+)?mod[ \t]+([A-Za-z_]\w*)[ \t]*;`)
	javaImport       = regexp.MustCompile(`(?m)^[ \t]*import[ \t]+(static[ \t]+)?([A-Za-z_][\w.]*?)(\.\*)?[ \t]*;`)
	jsRouteCall      = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*\.\s*(get|post|put|delete|patch|head|options|all)\s*\(`)
	pyRouteDecorator = regexp.MustCompile(`(?m)^[ \t]*@[ \t]*([A-Za-z_][\w.]*)[ \t]*\.[ \t]*(get|post|put|delete|patch|head|options|route|api_route|websocket)[ \t]*\(`)
	pyDefLine        = regexp.MustCompile(`^[ \t]*(?:async[ \t]+)?def[ \t]+([A-Za-z_]\w*)`)
	pyMethods        = regexp.MustCompile(`\bmethods\s*=\s*[\[(]`)
)

// extractImports lists import bindings in source order.
func extractImports(lang string, s *lexSource) []lexImport {
	switch lang {
	case "javascript", "typescript":
		return jsImports(s)
	case "python":
		return pyImports(s)
	case "rust":
		return rustImports(s)
	case "java":
		return javaImports(s)
	}
	return nil
}

func jsImports(s *lexSource) []lexImport {
	var out []lexImport
	for _, m := range jsStatementStart.FindAllStringSubmatchIndex(s.mask, -1) {
		start, keyword := m[2], s.mask[m[2]:m[3]]
		i := m[3]
		if keyword == "import" {
			if _, ok := s.punctAt(i, "("); ok {
				continue // dynamic import, handled below
			}
			out = append(out, jsImportClause(s, start, i, false)...)
			continue
		}
		// export ... from: re-exports are dependencies, not local bindings.
		j := s.skipSpace(i)
		if strings.HasPrefix(s.mask[j:], "*") || strings.HasPrefix(s.mask[j:], "{") || strings.HasPrefix(s.mask[j:], "type") {
			out = append(out, jsImportClause(s, start, i, true)...)
		}
	}
	for _, m := range jsRequire.FindAllStringSubmatchIndex(s.mask, -1) {
		specifier, end, ok := s.stringAt(m[1])
		if !ok {
			continue
		}
		if _, ok := s.punctAt(end, ")"); !ok {
			continue
		}
		binding := s.mask[m[2]:m[3]]
		if !strings.HasPrefix(binding, "{") {
			out = append(out, lexImport{offset: m[0], specifier: specifier, local: binding, kind: "namespace"})
			continue
		}
		for _, item := range strings.Split(strings.Trim(binding, "{} \t\r\n"), ",") {
			name, local, _ := strings.Cut(strings.TrimSpace(item), ":")
			name, local = strings.TrimSpace(name), strings.TrimSpace(local)
			if local == "" {
				local = name
			}
			if validLexIdent(name) && validLexIdent(local) {
				out = append(out, lexImport{offset: m[0], specifier: specifier, imported: name, local: local, kind: "named"})
			}
		}
	}
	for _, m := range jsDynamicImport.FindAllStringIndex(s.mask, -1) {
		if m[0] > 0 && (s.mask[m[0]-1] == '.' || s.mask[m[0]-1] == '$') {
			continue // obj.import(...) is a method call
		}
		if specifier, _, ok := s.stringAt(m[1]); ok {
			out = append(out, lexImport{offset: m[0], specifier: specifier, kind: "module"})
		}
	}
	return out
}

func validLexIdent(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !lexIdentByte(name[i], true) {
			return false
		}
	}
	return true
}

// jsImportClause parses `import a, {b as c} from 'x'`, `import * as n from
// 'x'`, `import 'x'` and the export-from forms.
func jsImportClause(s *lexSource, start, i int, reexport bool) []lexImport {
	limit := start + lexStatementLimit
	if specifier, _, ok := s.stringAt(i); ok {
		return []lexImport{{offset: start, specifier: specifier, kind: "module"}}
	}
	var bindings []lexImport
	if word, next := s.identAt(i); word == "type" {
		if after, _ := s.identAt(next); after != "from" && after != "" || strings.HasPrefix(s.mask[s.skipSpace(next):], "{") || strings.HasPrefix(s.mask[s.skipSpace(next):], "*") {
			i = next // TypeScript `import type ...`
		}
	}
	for i < limit && i < len(s.mask) {
		i = s.skipSpace(i)
		switch {
		case strings.HasPrefix(s.mask[i:], ","):
			i++
		case strings.HasPrefix(s.mask[i:], "*"):
			i++
			local := ""
			if word, next := s.identAt(i); word == "as" {
				local, i = s.identAt(next)
			}
			kind := "namespace"
			if reexport {
				kind = "reexport"
			}
			bindings = append(bindings, lexImport{offset: start, local: local, kind: kind})
		case strings.HasPrefix(s.mask[i:], "{"):
			end := s.matching(i, lexStatementLimit)
			if end < 0 {
				return nil
			}
			for _, item := range strings.Split(s.mask[i+1:end-1], ",") {
				fields := strings.Fields(item)
				if len(fields) > 0 && fields[0] == "type" {
					fields = fields[1:]
				}
				if len(fields) == 0 {
					continue
				}
				name, local := fields[0], fields[0]
				if len(fields) == 3 && fields[1] == "as" {
					local = fields[2]
				} else if len(fields) != 1 {
					continue
				}
				kind := "named"
				if reexport {
					kind = "reexport"
				}
				if name == "default" && !reexport {
					kind = "default"
				}
				if validLexIdent(name) && validLexIdent(local) {
					bindings = append(bindings, lexImport{offset: start, imported: name, local: local, kind: kind})
				}
			}
			i = end
		default:
			word, next := s.identAt(i)
			if word == "" {
				return nil
			}
			if word == "from" {
				specifier, _, ok := s.stringAt(next)
				if !ok {
					return nil
				}
				for index := range bindings {
					bindings[index].specifier = specifier
				}
				return bindings
			}
			if reexport {
				return nil
			}
			bindings = append(bindings, lexImport{offset: start, imported: "default", local: word, kind: "default"})
			i = next
		}
	}
	return nil
}

func pyImports(s *lexSource) []lexImport {
	var out []lexImport
	for _, m := range pyImport.FindAllStringSubmatchIndex(s.mask, -1) {
		for _, item := range strings.Split(s.mask[m[2]:m[3]], ",") {
			fields := strings.Fields(item)
			if len(fields) == 0 {
				continue
			}
			module := fields[0]
			if len(fields) == 3 && fields[1] == "as" {
				out = append(out, lexImport{offset: m[0], specifier: module, local: fields[2], kind: "namespace"})
			} else if len(fields) == 1 {
				// `import a.b` binds the top-level package name only.
				out = append(out, lexImport{offset: m[0], specifier: module, local: strings.Split(module, ".")[0], kind: "module"})
			}
		}
	}
	for _, m := range pyFromImport.FindAllStringSubmatchIndex(s.mask, -1) {
		module := s.mask[m[2]:m[3]]
		if m[4] >= 0 {
			module += s.mask[m[4]:m[5]]
		}
		i := m[1]
		var names string
		if strings.HasPrefix(s.mask[i:], "(") {
			end := s.matching(i, lexStatementLimit)
			if end < 0 {
				continue
			}
			names = s.mask[i+1 : end-1]
		} else {
			end := strings.IndexByte(s.mask[i:], '\n')
			if end < 0 {
				end = len(s.mask) - i
			}
			names = strings.TrimSuffix(strings.TrimSpace(s.mask[i:i+end]), "\\")
		}
		if strings.TrimSpace(names) == "*" {
			out = append(out, lexImport{offset: m[0], specifier: module, kind: "module"})
			continue
		}
		for _, item := range strings.Split(names, ",") {
			fields := strings.Fields(item)
			if len(fields) == 1 && validLexIdent(fields[0]) {
				out = append(out, lexImport{offset: m[0], specifier: module, imported: fields[0], local: fields[0], kind: "named"})
			} else if len(fields) == 3 && fields[1] == "as" && validLexIdent(fields[0]) && validLexIdent(fields[2]) {
				out = append(out, lexImport{offset: m[0], specifier: module, imported: fields[0], local: fields[2], kind: "named"})
			}
		}
	}
	return out
}

func rustImports(s *lexSource) []lexImport {
	var out []lexImport
	for _, m := range rustUse.FindAllStringIndex(s.mask, -1) {
		end := strings.IndexByte(s.mask[m[1]:min(len(s.mask), m[1]+lexStatementLimit)], ';')
		if end < 0 {
			continue
		}
		rustUseTree(normalizeRustUse(s.mask[m[1]:m[1]+end]), "", m[0], &out, 0)
	}
	for _, m := range rustMod.FindAllStringSubmatchIndex(s.mask, -1) {
		name := s.mask[m[2]:m[3]]
		out = append(out, lexImport{offset: m[0], specifier: "self", imported: name, local: name, kind: "mod"})
	}
	return out
}

// normalizeRustUse collapses whitespace in a use tree, keeping the single
// spaces that delimit `as`.
func normalizeRustUse(text string) string {
	tree := strings.Join(strings.Fields(text), " ")
	for _, token := range []string{"::", "{", "}", ","} {
		tree = strings.ReplaceAll(tree, " "+token, token)
		tree = strings.ReplaceAll(tree, token+" ", token)
	}
	return tree
}

// rustUseTree flattens `a::{b, c::d as e, f::*}` into bindings.
func rustUseTree(tree, prefix string, offset int, out *[]lexImport, depth int) {
	if depth > 8 || tree == "" {
		return
	}
	join := func(a, b string) string {
		if a == "" {
			return b
		}
		return a + "::" + b
	}
	if open := strings.IndexByte(tree, '{'); open >= 0 && strings.HasSuffix(tree, "}") {
		base := join(prefix, strings.TrimSuffix(tree[:open], "::"))
		level := 0
		inner := tree[open+1 : len(tree)-1]
		var part strings.Builder
		for i := 0; i < len(inner); i++ {
			switch inner[i] {
			case '{':
				level++
			case '}':
				level--
			case ',':
				if level == 0 {
					rustUseTree(part.String(), base, offset, out, depth+1)
					part.Reset()
					continue
				}
			}
			part.WriteByte(inner[i])
		}
		rustUseTree(part.String(), base, offset, out, depth+1)
		return
	}
	path, alias, _ := strings.Cut(tree, " as ")
	if alias != "" && !validLexIdent(alias) {
		return
	}
	full := join(prefix, path)
	if strings.HasSuffix(full, "::*") {
		*out = append(*out, lexImport{offset: offset, specifier: strings.TrimSuffix(full, "::*"), kind: "module"})
		return
	}
	module, name := "", full
	if i := strings.LastIndex(full, "::"); i >= 0 {
		module, name = full[:i], full[i+2:]
	}
	if name == "self" {
		if i := strings.LastIndex(module, "::"); i >= 0 {
			module, name = module[:i], module[i+2:]
		} else {
			module, name = "", module
		}
	}
	local := name
	if alias != "" {
		local = alias
	}
	if validLexIdent(name) && validLexIdent(local) {
		*out = append(*out, lexImport{offset: offset, specifier: module, imported: name, local: local, kind: "named"})
	}
}

func javaImports(s *lexSource) []lexImport {
	var out []lexImport
	for _, m := range javaImport.FindAllStringSubmatchIndex(s.mask, -1) {
		path := s.mask[m[4]:m[5]]
		if m[6] >= 0 {
			out = append(out, lexImport{offset: m[0], specifier: path, kind: "module"})
			continue
		}
		i := strings.LastIndexByte(path, '.')
		if i < 0 {
			continue
		}
		out = append(out, lexImport{offset: m[0], specifier: path[:i], imported: path[i+1:], local: path[i+1:], kind: "named"})
	}
	return out
}

// extractRoutes finds Express/Hono-style registrations in TypeScript and
// JavaScript and FastAPI/Flask decorators in Python. Only literal paths
// starting with "/" (or "*") are routes, which keeps map.get('k', d) and
// similar calls out.
func extractRoutes(lang string, s *lexSource) []lexRoute {
	switch lang {
	case "javascript", "typescript":
		return jsRoutes(s)
	case "python":
		return pyRoutes(s)
	}
	return nil
}

func routePath(path string) bool {
	return path == "*" || strings.HasPrefix(path, "/")
}

func jsRoutes(s *lexSource) []lexRoute {
	var out []lexRoute
	for _, m := range jsRouteCall.FindAllStringSubmatchIndex(s.mask, -1) {
		open := m[1] - 1
		if m[2] > 0 && lexIdentByte(s.mask[m[2]-1], true) {
			continue
		}
		path, after, ok := s.stringAt(open + 1)
		if !ok || !routePath(path) {
			continue
		}
		end := s.matching(open, lexStatementLimit)
		if end < 0 || after > end-1 {
			continue
		}
		args := splitTopLevel(s.mask[after : end-1])
		if len(args) < 2 || strings.TrimSpace(args[0]) != "" {
			continue
		}
		route := lexRoute{offset: m[0], method: strings.ToUpper(s.mask[m[4]:m[5]]), path: path}
		route.qualifier, route.handler = routeHandler(strings.TrimSpace(args[len(args)-1]), 1)
		out = append(out, route)
	}
	return out
}

// splitTopLevel splits masked argument text at depth-0 commas. The first
// element is whatever followed the already-consumed first argument.
func splitTopLevel(text string) []string {
	var parts []string
	depth, last := 0, 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, text[last:i])
				last = i + 1
			}
		}
	}
	return append(parts, text[last:])
}

var arrowDelegateCall = regexp.MustCompile(`^(?:async\s+)?(\([^()]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]*)?=>\s*(?:await\s+)?(?:([A-Za-z_$][\w$]*)\s*\.\s*)?([A-Za-z_$][\w$]*)\s*\(`)

// arrowDelegate names the function an inline arrow handler delegates to when
// its whole body is one call, as in (c) => roomsRoute(c.req.raw, c.env). A
// method of the arrow's own parameter (res.end(), c.text()) is the framework
// object, not a delegate.
func arrowDelegate(arg string) (string, string) {
	m := arrowDelegateCall.FindStringSubmatchIndex(arg)
	if m == nil {
		return "", ""
	}
	params := map[string]bool{}
	for _, word := range strings.FieldsFunc(arg[m[2]:m[3]], func(c rune) bool { return !lexIdentByte(byte(c), true) || c > 127 }) {
		params[word] = true
	}
	if (m[4] >= 0 && params[arg[m[4]:m[5]]]) || params[arg[m[6]:m[7]]] {
		return "", ""
	}
	depth := 0
	for i := m[1] - 1; i < len(arg); i++ {
		switch arg[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				if strings.TrimSpace(arg[i+1:]) != "" {
					return "", "" // more than one call
				}
				qualifier := ""
				if m[4] >= 0 {
					qualifier = arg[m[4]:m[5]]
				}
				return qualifier, arg[m[6]:m[7]]
			}
		}
	}
	return "", ""
}

var wrapperCall = regexp.MustCompile(`^(?:[A-Za-z_$][\w$]*\s*\.\s*)?[A-Za-z_$][\w$]*\s*\(`)

// routeHandler names a route's handler argument: a reference, a single-call
// arrow delegate, or (up to wrappers levels) the last argument of a
// middleware call such as guard(['POST'], handler).
func routeHandler(arg string, wrappers int) (string, string) {
	if qualifier, handler := handlerReference(arg); handler != "" {
		return qualifier, handler
	}
	if qualifier, handler := arrowDelegate(arg); handler != "" {
		return qualifier, handler
	}
	if m := wrapperCall.FindStringIndex(arg); m != nil && wrappers > 0 && strings.HasSuffix(arg, ")") {
		if inner := splitTopLevel(arg[m[1] : len(arg)-1]); strings.TrimSpace(inner[len(inner)-1]) != "" {
			return routeHandler(strings.TrimSpace(inner[len(inner)-1]), wrappers-1)
		}
	}
	return "", ""
}

// handlerReference accepts `name` and `object.name`; anything else is an
// inline handler.
func handlerReference(arg string) (string, string) {
	if validLexIdent(arg) {
		return "", arg
	}
	if object, name, ok := strings.Cut(arg, "."); ok && validLexIdent(strings.TrimSpace(object)) && validLexIdent(strings.TrimSpace(name)) {
		return strings.TrimSpace(object), strings.TrimSpace(name)
	}
	return "", ""
}

func pyRoutes(s *lexSource) []lexRoute {
	var out []lexRoute
	for _, m := range pyRouteDecorator.FindAllStringSubmatchIndex(s.mask, -1) {
		open := m[1] - 1
		path, _, ok := s.stringAt(open + 1)
		if !ok || !routePath(path) {
			continue
		}
		end := s.matching(open, lexStatementLimit)
		if end < 0 {
			continue
		}
		verb := s.mask[m[4]:m[5]]
		methods := []string{strings.ToUpper(verb)}
		switch verb {
		case "route", "api_route":
			methods = []string{"GET"}
			if loc := pyMethods.FindStringIndex(s.mask[open:end]); loc != nil {
				listOpen := open + loc[1] - 1
				listEnd := s.matching(listOpen, lexStatementLimit)
				if listEnd > 0 {
					methods = nil
					for i := listOpen + 1; i < listEnd-1; {
						value, next, ok := s.stringAt(i)
						if ok {
							methods = append(methods, strings.ToUpper(value))
							i = next
							continue
						}
						i++
					}
				}
			}
		case "websocket":
			methods = []string{"WS"}
		}
		// The handler is the first def after this (possibly multi-line)
		// decorator, past any further decorators.
		closing, _ := s.position(end - 1)
		decorated := 0
		for next := closing + 1; next <= len(s.lines) && next <= closing+lexDecoratorWindow; next++ {
			text := strings.TrimSpace(s.lineText(next))
			if text == "" {
				continue
			}
			if strings.HasPrefix(text, "@") {
				if open := strings.IndexByte(s.lineText(next), '('); open >= 0 {
					if stop := s.matching(s.lines[next-1]+open, lexStatementLimit); stop > 0 {
						next, _ = s.position(stop - 1)
					}
				}
				continue
			}
			if pyDefLine.MatchString(s.lineText(next)) {
				decorated = next
			}
			break
		}
		if decorated == 0 {
			continue
		}
		name := pyDefLine.FindStringSubmatch(s.lineText(decorated))[1]
		for _, method := range methods {
			if method != "" {
				out = append(out, lexRoute{offset: m[0], method: method, path: path, handler: name, decorated: decorated})
			}
		}
	}
	return out
}

// lineText returns the masked text of a 1-based line.
func (s *lexSource) lineText(line int) string {
	if line < 1 || line > len(s.lines) {
		return ""
	}
	start := s.lines[line-1]
	end := len(s.mask)
	if line < len(s.lines) {
		end = s.lines[line] - 1
	}
	return s.mask[start:end]
}

// writeLexRelations records imports and routes. enclosing names the function
// that contains an offset, or returns empty strings at module scope.
func writeLexRelations(lang, repo, rel string, s *lexSource, db *indexWriter, enclosing func(int) (string, string)) {
	if lang == "" {
		return
	}
	for _, imp := range extractImports(lang, s) {
		line, column := s.position(imp.offset)
		to := imp.imported
		if to == "" || imp.kind == "default" {
			to = imp.specifier
		}
		id := rel + "#import:" + itoa(line) + ":" + itoa(column) + ":" + imp.kind + ":" + imp.local + ":" + imp.imported
		db.relation(repo, rel, id, "", rel, to, "import", line, strings.Join([]string{"leximport", lang, imp.specifier, imp.imported, imp.local, imp.kind}, "\x1f"))
	}
	for _, route := range extractRoutes(lang, s) {
		line, column := s.position(route.offset)
		fromID, _ := enclosing(route.offset)
		handler := route.handler
		if handler == "" {
			handler = "(inline)"
		}
		id := rel + "#route:" + itoa(line) + ":" + itoa(column) + ":" + route.method
		db.relation(repo, rel, id, fromID, route.method+" "+route.path, handler, "route", line, strings.Join([]string{"lexroute", route.qualifier, route.handler, itoa(route.decorated)}, "\x1f"))
	}
}
