//go:build !treesitter

package graph

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Declaration-aware lexical analysis for TypeScript/JavaScript, Python, Rust
// and Java. Declarations get extents (braces or indentation over masked
// source), so a symbol's source hash covers its body and calls are
// attributed to the innermost enclosing function. Calls, imports, routes and
// type relationships are written with hints that the repository-wide pass in
// lexresolve.go resolves. A call whose name (or qualifier) is a parameter or
// local of the calling function is marked local and never resolved.

type lexDecl struct {
	name, kind, container string
	signature             string
	line, endLine         int
	offset                int // start of the declaration line
	paramStart            int // offset just past the declared name
	bodyStart, bodyEnd    int // body byte range; bodyStart < 0 when bodyless
	callable, scope       bool
	static                bool // statically bound member
	exported              bool // JavaScript/TypeScript module export
	supers                []lexSuper
	locals                map[string]bool
	bodyDepth             int // brace depth before the body opens
	id                    string
}

type lexSuper struct{ kind, qualifier, name string }

type lexPattern struct {
	re       *regexp.Regexp
	kind     string // func or type
	callable bool
	scope    bool // members are declared directly inside its body
	member   bool // only matches directly inside a class or impl body
}

const (
	jsModifiers    = `(?:(?:public|private|protected|static|async|override|readonly|abstract|declare|get|set|accessor)\s+)*`
	javaModifiers  = `(?:@[\w.]+(?:\([^)]*\))?\s+)*(?:(?:public|protected|private|abstract|static|final|synchronized|native|default|strictfp|transient|sealed|non-sealed)\s+)*`
	rustVisibility = `(?:pub(?:\([^)]*\))?\s+)?`
)

var jsPatterns = []lexPattern{
	{re: regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`), kind: "func", callable: true},
	{re: regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), kind: "type", scope: true},
	{re: regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)`), kind: "type"},
	{re: regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`), kind: "type"},
	{re: regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?type\s+([A-Za-z_$][\w$]*)\s*(?:<[^=]*>)?\s*=`), kind: "type"},
	{re: regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:function\b|(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=>)`), kind: "func", callable: true},
	{re: regexp.MustCompile(`^\s*` + jsModifiers + `\*?\s*(#?[A-Za-z_$][\w$]*)\s*(?:<[^<>()]*>)?\s*\(`), kind: "func", callable: true, member: true},
	{re: regexp.MustCompile(`^\s*` + jsModifiers + `(#?[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*(?::[^=]+)?=>`), kind: "func", callable: true, member: true},
}

var lexPatterns = map[string][]lexPattern{
	"javascript": jsPatterns,
	"typescript": jsPatterns,
	"python": {
		{re: regexp.MustCompile(`^[ \t]*(?:async[ \t]+)?def[ \t]+([A-Za-z_]\w*)`), kind: "func", callable: true},
		{re: regexp.MustCompile(`^[ \t]*class[ \t]+([A-Za-z_]\w*)`), kind: "type", scope: true},
	},
	"rust": {
		{re: regexp.MustCompile(`^\s*` + rustVisibility + `(?:(?:default|const|async|unsafe|extern)\s+)*fn\s+([A-Za-z_]\w*)`), kind: "func", callable: true},
		{re: regexp.MustCompile(`^\s*` + rustVisibility + `(?:struct|enum|union|type)\s+([A-Za-z_]\w*)`), kind: "type"},
		{re: regexp.MustCompile(`^\s*` + rustVisibility + `(?:unsafe\s+)?trait\s+([A-Za-z_]\w*)`), kind: "type", scope: true},
	},
	"java": {
		{re: regexp.MustCompile(`^\s*` + javaModifiers + `(?:class|interface|enum|record|@interface)\s+([A-Za-z_$][\w$]*)`), kind: "type", scope: true},
		{re: regexp.MustCompile(`^\s*` + javaModifiers + `(?:<[^>]+>\s+)?(?:[\w$.]+(?:<[^;{}()=]*>)?(?:\[\])*\s+)?([A-Za-z_$][\w$]*)\s*\(`), kind: "func", callable: true, member: true},
	},
}

var (
	rustInlineModLine = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+([A-Za-z_]\w*)\s*\{`)
	jsExportedLine    = regexp.MustCompile(`^\s*export\b`)
	jsExportList      = regexp.MustCompile(`(?m)^[ \t]*export[ \t]*\{([^}]*)\}[ \t]*;?[ \t]*$`)
	jsExportDefault   = regexp.MustCompile(`(?m)^[ \t]*export[ \t]+default[ \t]+([A-Za-z_$][\w$]*)[ \t]*;?[ \t]*$`)
	cjsExportAssign   = regexp.MustCompile(`(?m)^[ \t]*(?:module\.)?exports\.([A-Za-z_$][\w$]*)[ \t]*=[ \t]*([A-Za-z_$][\w$]*)[ \t]*;?[ \t]*$`)
	cjsExportObject   = regexp.MustCompile(`\bmodule\.exports\s*=\s*\{`)
	rustImpl          = regexp.MustCompile(`^\s*(?:unsafe\s+)?impl\b(?:\s*<[^{]*?>)?\s+(?:([A-Za-z_][\w:]*)(?:<[^{]*?>)?\s+for\s+)?([A-Za-z_][\w:]*)`)
	heritageWord      = regexp.MustCompile(`\b(extends|implements|permits)\b`)
	pyBases           = regexp.MustCompile(`^[ \t]*class[ \t]+[A-Za-z_]\w*[ \t]*\(`)
	lexStaticWord     = regexp.MustCompile(`\b(?:static|private)\b`)
	javaModifier      = wordSet("public protected private abstract static final synchronized native default strictfp transient sealed non-sealed")
)

var lexKeywords = map[string]map[string]bool{
	"javascript": wordSet("if for while switch catch return function typeof new delete void await yield super import require else do in of instanceof case throw with class"),
	"python":     wordSet("if elif while for return def class lambda print assert del not and or in is yield await with except raise global nonlocal super self cls"),
	"rust":       wordSet("if while for match return fn loop let impl where move as in mut ref unsafe Some None Ok Err Box Vec self Self super crate"),
	"java":       wordSet("if for while switch catch return new throw synchronized super this else do try case assert instanceof"),
}

func init() {
	lexKeywords["typescript"] = lexKeywords["javascript"]
	lexCallPatterns["typescript"] = lexCallPatterns["javascript"]
}

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

// lexDeclarationLanguage reports languages with declaration-aware analysis.
func lexDeclarationLanguage(lang string) bool { _, ok := lexPatterns[lang]; return ok }

func analyzeLexicalFile(lang, repo, rel, src string, out *fileBatch) (int, error) {
	s := newLexSource(lang, src)
	decls := lexDeclarations(lang, s)
	// Hashes keep string literals: a literal-only edit is a source change.
	hashText := maskSource(lang, src, true)
	for _, d := range decls {
		d.id = goSymbolID(rel, d.name, d.line)
		out.namespace = lexNamespace(rel, d.container, d.static, d.exported)
		out.symbol(d.id, d.name, d.kind, d.line, d.signature)
		out.content(d.id, lexContent(s, hashText, d), d.endLine)
	}
	out.namespace = lexNamespace(rel, "", false, false)
	for _, d := range decls {
		for _, super := range d.supers {
			id := d.id + "->" + super.kind + ":" + super.qualifier + "." + super.name
			out.relation(id, d.id, d.name, super.name, super.kind, d.line, "lextype\x1f"+super.qualifier)
		}
	}
	for _, call := range lexCalls(lang, s, decls) {
		line, column := s.position(call.offset)
		out.relation(goRelationID(call.from.id, call.name, line, column), call.from.id, call.from.name, call.name, "call", line, call.hint)
	}
	var callables []*lexDecl
	for _, d := range decls {
		if d.callable && d.bodyStart >= 0 {
			callables = append(callables, d)
		}
	}
	writeLexRelations(lang, repo, rel, s, out, func(offset int) (string, string) {
		if from := innermost(callables, offset); from != nil {
			return from.id, from.name
		}
		return "", ""
	}, func(offset int, name string) bool {
		var chain []*lexDecl
		for _, d := range callables {
			if offset > d.bodyStart && offset < d.bodyEnd {
				chain = append(chain, d)
			}
		}
		return lexBound(chain, name)
	})
	return len(decls), nil
}

// lexNamespace identifies a symbol's file, container and binding flags for
// the lexical resolver. It never contains '|', so Go resolution can never
// match it.
func lexNamespace(rel, container string, static, exported bool) string {
	namespace := "lex:" + rel + "#" + container
	if static {
		namespace += "#static"
	}
	if exported {
		namespace += "#export"
	}
	return namespace
}

// lexContent is the comment-masked declaration and body with whitespace
// runs collapsed, so comment and formatting edits do not change its hash.
func lexContent(s *lexSource, hashText string, d *lexDecl) []byte {
	end := len(s.mask)
	if d.bodyEnd > 0 {
		end = d.bodyEnd
	} else if d.endLine < len(s.lines) {
		end = s.lines[d.endLine] - 1
	}
	if end < d.offset {
		end = d.offset
	}
	return []byte(strings.Join(strings.Fields(hashText[d.offset:end]), " "))
}

// lexDeclarations finds declarations line by line. Member patterns apply
// only directly inside a class/impl body; a declaration inside a function
// body is local. Bodies nest or are disjoint, so the enclosing bodies of the
// current line form a stack and the whole scan stays linear.
func lexDeclarations(lang string, s *lexSource) []*lexDecl {
	depths := lineDepths(s)
	var decls, scopes, open []*lexDecl
	seen := map[string]bool{}
	push := func(d *lexDecl) {
		if d.bodyStart >= 0 {
			d.bodyDepth = depthAt(s, depths, d.bodyStart)
			open = append(open, d)
		}
	}
	for line := 1; line <= len(s.lines); line++ {
		text := s.lineText(line)
		start := s.lines[line-1]
		for len(open) > 0 && open[len(open)-1].bodyEnd <= start {
			open = open[:len(open)-1]
		}
		if lang == "rust" {
			if m := rustInlineModLine.FindStringSubmatch(text); m != nil {
				// Items of an inline module are not items of the file's
				// module, so the block is a scope of its own.
				module := &lexDecl{name: "mod:" + m[1], kind: "mod", offset: start, line: line, scope: true}
				lexBody(lang, s, module, start+len(m[0])-1)
				scopes = append(scopes, module)
				push(module)
				continue
			}
			if m := rustImpl.FindStringSubmatch(text); m != nil {
				impl := &lexDecl{name: lastPathSegment(m[2]), kind: "impl", offset: start, line: line, scope: true}
				lexBody(lang, s, impl, start+len(m[0]))
				if m[1] != "" {
					qualifier, name := splitRustPath(m[1])
					impl.supers = append(impl.supers, lexSuper{kind: "implements", qualifier: qualifier, name: name})
				}
				scopes = append(scopes, impl)
				push(impl)
				continue
			}
		}
		var enclosing, scope *lexDecl
		for i := len(open) - 1; i >= 0; i-- {
			if open[i].bodyStart < start && start < open[i].bodyEnd {
				if enclosing == nil {
					enclosing = open[i]
				}
				if open[i].scope {
					scope = open[i]
					break
				}
			}
		}
		if scope != nil && lang != "python" && depths[line-1] != scope.bodyDepth+1 {
			scope = nil // nested deeper, for example inside a method body
		}
		for _, p := range lexPatterns[lang] {
			m := p.re.FindStringSubmatchIndex(text)
			if m == nil {
				continue
			}
			name := text[m[2]:m[3]]
			if lexKeywords[lang][name] {
				break
			}
			if p.member && scope == nil {
				continue // a method shape outside a class body is a call
			}
			if lang == "java" && p.member && !javaMethodShape(text[:m[2]], name, scope.name) {
				break
			}
			d := &lexDecl{name: name, kind: p.kind, offset: start, paramStart: start + m[3], line: line, callable: p.callable, scope: p.scope, signature: strings.TrimSpace(text)}
			d.exported = (lang == "javascript" || lang == "typescript") && jsExportedLine.MatchString(text)
			if scope != nil {
				d.container = scope.name
				switch {
				case lang == "rust":
					d.static = scope.kind == "impl"
				default:
					d.static = lexStaticWord.MatchString(text[:m[2]])
				}
			}
			if enclosing != nil && enclosing.callable && (scope == nil || enclosing.bodyStart > scope.bodyStart) {
				d.container, d.static = lexLocalContainer, false
			}
			if lang == "python" {
				pyBody(s, d, line)
			} else {
				lexBody(lang, s, d, start+m[3])
				if d.callable && d.bodyStart < 0 && (lang == "javascript" || lang == "typescript") {
					lexOverloadBody(lang, s, d, text, start, m[3])
				}
			}
			d.supers = lexHeritage(lang, text, m[3])
			key := d.name + "\x00" + strconv.Itoa(d.line)
			if seen[key] {
				break // one identity per name and line
			}
			seen[key] = true
			decls = append(decls, d)
			if d.scope {
				scopes = append(scopes, d)
			}
			push(d)
			break
		}
	}
	if lang == "javascript" || lang == "typescript" {
		// `export { a, b as c }` and `export default name` export earlier
		// module-level declarations.
		exported := map[string]bool{}
		for _, m := range jsExportList.FindAllStringSubmatch(s.mask, -1) {
			for _, item := range strings.Split(m[1], ",") {
				if fields := strings.Fields(item); len(fields) > 0 && fields[0] != "type" {
					exported[fields[0]] = true
				}
			}
		}
		for _, m := range jsExportDefault.FindAllStringSubmatch(s.mask, -1) {
			exported[m[1]] = true
		}
		// CommonJS: `exports.f = f` and shorthand `module.exports = { f }`
		// entries export the local f; `{ f: g }` exports g as f and is
		// left out.
		for _, m := range cjsExportAssign.FindAllStringSubmatch(s.mask, -1) {
			if m[1] == m[2] {
				exported[m[1]] = true
			}
		}
		for _, m := range cjsExportObject.FindAllStringIndex(s.mask, -1) {
			if end := s.closeBrace(m[1] - 1); end > 0 {
				for _, entry := range splitTopLevel(s.mask[m[1] : end-1]) {
					key, value, found := strings.Cut(entry, ":")
					key, value = strings.TrimSpace(key), strings.TrimSpace(value)
					if validLexIdent(key) && (!found || key == value) {
						exported[key] = true
					}
				}
			}
		}
		for _, d := range decls {
			if d.container == "" && exported[d.name] {
				d.exported = true
			}
		}
	}
	assigned := lexAssignments(lang, s)
	for _, d := range decls {
		if d.callable && d.container == "" {
			for _, line := range assigned[d.name] {
				if line != d.line {
					// A reassigned binding holds a function value: calls
					// through it are dynamic, so it is never a target.
					d.container = lexLocalContainer
					break
				}
			}
		}
	}
	types := map[string]*lexDecl{}
	for _, d := range decls {
		if d.kind == "type" && d.container == "" && types[d.name] == nil {
			types[d.name] = d
		}
	}
	for _, scope := range scopes {
		if scope.kind == "impl" && len(scope.supers) > 0 && types[scope.name] != nil {
			// `impl Trait for Type` relates Type to Trait.
			types[scope.name].supers = append(types[scope.name].supers, scope.supers...)
		}
	}
	sort.SliceStable(decls, func(i, j int) bool { return decls[i].offset < decls[j].offset })
	return decls
}

var lexAssignmentPatterns = map[string]*regexp.Regexp{
	"javascript": regexp.MustCompile(`(?m)(?:^|[^.\w$])([A-Za-z_$][\w$]*)\s*(?:[-+*/|&]|\?\?|\|\||&&)?=(?:[^=>]|$)`),
	"typescript": regexp.MustCompile(`(?m)(?:^|[^.\w$])([A-Za-z_$][\w$]*)\s*(?:[-+*/|&]|\?\?|\|\||&&)?=(?:[^=>]|$)`),
	"python":     regexp.MustCompile(`(?m)^([A-Za-z_]\w*)\s*(?::[^=\n]+)?=[^=]`),
}

// lexAssignments maps each assigned name to the lines assigning it, in one
// pass over the file (JavaScript/TypeScript anywhere, Python at module level).
func lexAssignments(lang string, s *lexSource) map[string][]int {
	assigned := map[string][]int{}
	if pattern := lexAssignmentPatterns[lang]; pattern != nil {
		for _, m := range pattern.FindAllStringSubmatchIndex(s.mask, -1) {
			line, _ := s.position(m[2])
			assigned[s.mask[m[2]:m[3]]] = append(assigned[s.mask[m[2]:m[3]]], line)
		}
	}
	return assigned
}

// javaMethodShape rejects statements that look like declarations: a member
// without a return type must be a constructor of its class.
func javaMethodShape(head, name, container string) bool {
	types := 0
	for _, word := range strings.Fields(head) {
		if lexKeywords["java"][word] || strings.ContainsAny(word, "=+") {
			return false
		}
		if !javaModifier[word] && !strings.HasPrefix(word, "@") {
			types++
		}
	}
	return types > 0 || name == container
}

// lineDepths records the brace depth at the start of each line.
func lineDepths(s *lexSource) []int {
	depths := make([]int, len(s.lines))
	level, line := 0, 0
	for i := 0; i <= len(s.mask); i++ {
		for line < len(s.lines) && s.lines[line] == i {
			depths[line] = level
			line++
		}
		if i == len(s.mask) {
			break
		}
		switch s.mask[i] {
		case '{':
			level++
		case '}':
			if level > 0 {
				level--
			}
		}
	}
	return depths
}

// depthAt is the brace depth just before offset.
func depthAt(s *lexSource, depths []int, offset int) int {
	line, _ := s.position(offset)
	level := depths[line-1]
	for i := s.lines[line-1]; i < offset; i++ {
		switch s.mask[i] {
		case '{':
			level++
		case '}':
			if level > 0 {
				level--
			}
		}
	}
	return level
}

// lexBody finds a declaration body after its name: the brace body that
// starts before a statement-ending ';', or the expression after an arrow.
// Once the declaration's brackets are balanced, a line break ends the search
// unless the next line continues the declaration. A bodyless declaration (a
// TypeScript overload or a type alias written without semicolons) therefore
// never takes a later declaration's body, and each search is bounded.
// lexOverloadBody gives a bodyless TypeScript function declaration the body
// of a later declaration of the same name on its line, as in
// `function f(a: string): void; function f(a: any) { ... }`. The line holds
// one identity, which then describes the implementation rather than the
// overload signature.
func lexOverloadBody(lang string, s *lexSource, d *lexDecl, text string, start, after int) {
	rest := text[after:]
	if !strings.Contains(rest, "function") {
		return
	}
	later := regexp.MustCompile(`\bfunction\s*\*?\s*` + regexp.QuoteMeta(d.name))
	for _, m := range later.FindAllStringIndex(rest, -1) {
		if m[1] < len(rest) && lexIdentByte(rest[m[1]], true) {
			continue // a longer name
		}
		candidate := *d
		lexBody(lang, s, &candidate, start+after+m[1])
		if candidate.bodyStart >= 0 {
			d.paramStart = start + after + m[1]
			d.bodyStart, d.bodyEnd, d.endLine = candidate.bodyStart, candidate.bodyEnd, candidate.endLine
			return
		}
	}
}

func lexBody(lang string, s *lexSource, d *lexDecl, from int) {
	d.bodyStart, d.bodyEnd, d.endLine = -1, -1, d.line
	parens, arrow := 0, -1
	for i := from; i < len(s.mask) && i-from < lexStatementLimit; i++ {
		c := s.mask[i]
		switch {
		case c == '(' || c == '[':
			parens++
		case c == ')' || c == ']':
			parens--
		case parens > 0:
		case c == '=' && i+1 < len(s.mask) && s.mask[i+1] == '>':
			arrow = i + 2
			i++
		case c == '{':
			end := s.closeBrace(i)
			if end < 0 {
				return
			}
			d.bodyStart, d.bodyEnd = i, end
			d.endLine, _ = s.position(end - 1)
			return
		case c == ';':
			if arrow >= 0 {
				d.bodyStart, d.bodyEnd = arrow, i
			}
			d.endLine, _ = s.position(i)
			return
		case c == '\n':
			if arrow >= 0 && strings.TrimSpace(s.mask[arrow:i]) != "" {
				d.bodyStart, d.bodyEnd = arrow, i
				d.endLine, _ = s.position(i)
				return
			}
			if arrow < 0 && !lexContinues(s, i) {
				d.endLine, _ = s.position(i)
				return
			}
		}
	}
}

var (
	lexContinuationSuffixes = []string{",", "(", "[", "=", "|", "&", "<", ":", "=>", "->", "+", "-", "*", ".", "?", "extends", "implements", "throws"}
	lexContinuationPrefixes = []string{"{", "|", "&", ".", "?", ":", "=>", "->", "where", "throws", "extends", "implements", ")", ">", ",", "+", "-", "="}
)

// lexContinues reports whether a declaration continues past the line break
// at i: its line ends with an operator or opening bracket, or the next
// non-blank line opens the body or continues a clause. String literals
// count as operands, so `type Mode = 'a' | 'b'` ends with its line.
func lexContinues(s *lexSource, i int) bool {
	line, _ := s.position(i)
	if text := strings.TrimRight(s.codeLine(line), " \t\r"); text != "" {
		for _, suffix := range lexContinuationSuffixes {
			if strings.HasSuffix(text, suffix) {
				return true
			}
		}
	}
	for next := line + 1; next <= len(s.lines) && next <= line+8; next++ {
		text := strings.TrimSpace(s.codeLine(next))
		if text == "" {
			continue
		}
		for _, prefix := range lexContinuationPrefixes {
			if strings.HasPrefix(text, prefix) {
				return true
			}
		}
		return false
	}
	return false
}

// pyBody extends a Python def or class over its indented block, past a
// multi-line signature.
func pyBody(s *lexSource, d *lexDecl, line int) {
	head := s.lineText(line)
	indent := len(head) - len(strings.TrimLeft(head, " \t"))
	parens := strings.Count(head, "(") - strings.Count(head, ")")
	end := line
	for next := line + 1; next <= len(s.lines); next++ {
		text := s.lineText(next)
		if parens > 0 {
			parens += strings.Count(text, "(") - strings.Count(text, ")")
			end = next
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(text)-len(strings.TrimLeft(text, " \t")) <= indent {
			break
		}
		end = next
	}
	d.endLine = end
	d.bodyStart, d.bodyEnd = s.lines[line-1], len(s.mask)
	if end < len(s.lines) {
		d.bodyEnd = s.lines[end] - 1
	}
}

// lexHeritage reads extends/implements clauses of a class declaration line.
func lexHeritage(lang, text string, nameEnd int) []lexSuper {
	var supers []lexSuper
	add := func(kind, list string) {
		for _, item := range splitTopLevel(list) {
			item = strings.TrimSpace(item)
			if i := strings.IndexAny(item, "<("); i >= 0 {
				item = strings.TrimSpace(item[:i])
			}
			qualifier, name := "", item
			if i := strings.LastIndexByte(item, '.'); i >= 0 {
				qualifier, name = item[:i], item[i+1:]
			}
			if validLexIdent(name) && (qualifier == "" || validLexIdent(qualifier)) && name != "object" && name != "Object" {
				supers = append(supers, lexSuper{kind: kind, qualifier: qualifier, name: name})
			}
		}
	}
	tail := text[nameEnd:]
	switch lang {
	case "python":
		if pyBases.MatchString(text) {
			open := strings.IndexByte(text, '(')
			if close := strings.LastIndexByte(text, ')'); close > open {
				var bases []string
				for _, base := range splitTopLevel(text[open+1 : close]) {
					if !strings.Contains(base, "=") {
						bases = append(bases, base)
					}
				}
				add("extends", strings.Join(bases, ","))
			}
		}
	case "javascript", "typescript", "java":
		if brace := strings.IndexByte(tail, '{'); brace >= 0 {
			tail = tail[:brace]
		}
		words := heritageWord.FindAllStringSubmatchIndex(tail, -1)
		for i, m := range words {
			end := len(tail)
			if i+1 < len(words) {
				end = words[i+1][0]
			}
			if kind := tail[m[2]:m[3]]; kind != "permits" {
				add(kind, tail[m[1]:end])
			}
		}
	}
	return supers
}

type lexCall struct {
	offset int
	from   *lexDecl
	name   string
	hint   string
}

var lexCallPatterns = map[string]*regexp.Regexp{
	"javascript": regexp.MustCompile(`(?:([A-Za-z_$][\w$]*)\s*\.\s*)?([A-Za-z_$][\w$]*)\s*(?:<[^<>()]*>)?\s*\(`),
	"python":     regexp.MustCompile(`(?:([A-Za-z_]\w*)\s*\.\s*)?([A-Za-z_]\w*)\s*\(`),
	"rust":       regexp.MustCompile(`((?:[A-Za-z_]\w*::)+)?([A-Za-z_]\w*)\s*(?:::\s*<[^()]*>)?\s*\(`),
	"java":       regexp.MustCompile(`(?:([A-Za-z_$][\w$]*)\s*\.\s*)?([A-Za-z_$][\w$]*)\s*\(`),
}

// lexCalls attributes call sites to the innermost enclosing callable. Member
// calls on expressions (a.b.c(), f().g()), constructor calls and calls on
// this/self are not observed: their targets are dynamic.
func lexCalls(lang string, s *lexSource, decls []*lexDecl) []lexCall {
	var callables []*lexDecl
	for _, d := range decls {
		if d.callable && d.bodyStart >= 0 {
			callables = append(callables, d)
		}
	}
	lexAssignLocals(lang, s, callables)
	var modules [][2]int
	if lang == "rust" {
		modules = rustInlineMods(s)
	}
	// Calls arrive in offset order, so the bodies enclosing the current call
	// form a stack (innermost last).
	chainAt := lexSweep(callables, func(d *lexDecl) int { return d.bodyStart })
	var calls []lexCall
	for _, m := range lexCallPatterns[lang].FindAllStringSubmatchIndex(s.mask, -1) {
		start := m[0]
		if start > 0 {
			before := s.mask[start-1]
			if before == '.' || lexIdentByte(before, true) || before == ':' || before == '#' {
				continue
			}
		}
		name := s.mask[m[4]:m[5]]
		qualifier := ""
		if m[2] >= 0 {
			qualifier = strings.TrimSuffix(s.mask[m[2]:m[3]], "::")
		}
		if lexKeywords[lang][name] || qualifier == "this" || qualifier == "self" || qualifier == "cls" || qualifier == "super" {
			continue
		}
		if prefix := strings.TrimRight(s.mask[max(0, start-9):start], " \t"); strings.HasSuffix(prefix, "new") || strings.HasSuffix(prefix, "def") || strings.HasSuffix(prefix, "fn") || strings.HasSuffix(prefix, "function") || strings.HasSuffix(prefix, "class") {
			continue
		}
		chain := chainAt(start)
		if len(chain) == 0 {
			continue
		}
		from := chain[len(chain)-1]
		if start < from.paramStart && name == from.name {
			continue
		}
		if line, _ := s.position(start); line == from.line && name == from.name {
			continue // the declaration itself
		}
		local := name
		if qualifier != "" {
			local = strings.Split(qualifier, "::")[0]
		}
		hint := "lexcall\x1f" + qualifier + "\x1f" + from.container
		// Java method names and variables are separate namespaces, so there
		// only qualifiers can be shadowed. Inside a Rust inline module only
		// absolute crate:: paths are resolved: its scope is not the file's.
		if (lang != "java" || qualifier != "") && lexBound(chain, local) {
			hint = "lexlocal"
		}
		if insideRanges(modules, start) && !strings.HasPrefix(qualifier, "crate") {
			hint = "lexlocal"
		}
		calls = append(calls, lexCall{offset: start, from: from, name: name, hint: hint})
	}
	return calls
}

func innermost(callables []*lexDecl, offset int) *lexDecl {
	var best *lexDecl
	for _, d := range callables {
		if d.bodyStart >= 0 && offset > d.bodyStart && offset < d.bodyEnd && (best == nil || d.bodyStart > best.bodyStart) {
			best = d
		}
	}
	return best
}

var (
	lexIdentifier = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
	lexLocalDecls = map[string][]*regexp.Regexp{
		"javascript": {
			regexp.MustCompile(`\b(?:const|let|var)\s+([^;\n]+)`),
			regexp.MustCompile(`\b(?:function|class)\s*\*?\s*([A-Za-z_$][\w$]*)`),
			regexp.MustCompile(`\bcatch\s*\(\s*([^)]*)\)`),
			regexp.MustCompile(`\(([^()]*)\)\s*(?::[^=>{]*)?=>`),
			regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*=>`),
		},
		"python": {
			regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_][\w, \t\[\]()*]*?)\s*(?::[^=\n]+)?=[^=]`),
			regexp.MustCompile(`\bfor\s+([^:\n]+?)\s+in\b`),
			regexp.MustCompile(`\bas\s+([A-Za-z_]\w*)`),
			regexp.MustCompile(`\b(?:global|nonlocal|import)\s+([^\n]+)`),
			regexp.MustCompile(`\b(?:def|class)\s+([A-Za-z_]\w*)`),
			regexp.MustCompile(`\blambda\s+([^:]*):`),
		},
		"rust": {
			regexp.MustCompile(`\blet\s+([^=;]+)`),
			regexp.MustCompile(`\|([^|]*)\|`),
			regexp.MustCompile(`\bfor\s+([^{]+?)\s+in\b`),
			regexp.MustCompile(`\bfn\s+([A-Za-z_]\w*)`),
		},
		"java": {
			regexp.MustCompile(`\b[\w$.<>\[\]]+\s+([a-z_$][\w$]*)\s*(?:[=;:,)])`),
			regexp.MustCompile(`\(([^()]*)\)\s*->`),
			regexp.MustCompile(`\b([a-z_$][\w$]*)\s*->`),
		},
	}
)

func init() { lexLocalDecls["typescript"] = lexLocalDecls["javascript"] }

// lexSweep answers "which callables contain this offset" for non-decreasing
// offsets in linear total time. start gives the offset a callable's scope
// begins at; scopes nest or are disjoint.
func lexSweep(callables []*lexDecl, start func(*lexDecl) int) func(int) []*lexDecl {
	sorted := append([]*lexDecl(nil), callables...)
	sort.SliceStable(sorted, func(i, j int) bool { return start(sorted[i]) < start(sorted[j]) })
	var open []*lexDecl
	next := 0
	return func(offset int) []*lexDecl {
		for next < len(sorted) && start(sorted[next]) < offset {
			open = append(open, sorted[next])
			next++
		}
		kept := open[:0]
		for _, d := range open {
			if d.bodyEnd > offset {
				kept = append(kept, d)
			}
		}
		open = kept
		var chain []*lexDecl
		for _, d := range open {
			if offset > start(d) && offset < d.bodyEnd {
				chain = append(chain, d)
			}
		}
		return chain
	}
}

// lexAssignLocals records every parameter and local binding once, on the
// innermost function whose scope contains it. A name is then local to a call
// when any function enclosing the call binds it, which also covers closures
// capturing an outer parameter. Over-approximation only leaves calls
// unresolved.
func lexAssignLocals(lang string, s *lexSource, callables []*lexDecl) {
	// A Python function's scope starts after its name: the def line holds
	// its own name (bound in the enclosing scope) and its parameters.
	scopeStart := func(d *lexDecl) int {
		if lang == "python" {
			return d.paramStart
		}
		return d.bodyStart
	}
	for _, d := range callables {
		d.locals = map[string]bool{}
		header := s.mask[d.paramStart:max(d.paramStart, min(len(s.mask), d.paramStart+lexStatementLimit))]
		if open := strings.IndexByte(header, '('); open >= 0 && (d.bodyStart < 0 || d.paramStart+open < d.bodyStart || lang == "python") {
			if close := s.matching(d.paramStart+open, lexStatementLimit); close > 0 {
				for _, param := range splitTopLevel(s.mask[d.paramStart+open+1 : close-1]) {
					for _, word := range lexParamNames(lang, param) {
						d.locals[word] = true
					}
				}
			}
		} else if arrow := strings.Index(header, "=>"); arrow >= 0 {
			for _, word := range lexIdentifier.FindAllString(header[:arrow], -1) {
				d.locals[word] = true
			}
		}
	}
	for _, pattern := range lexLocalDecls[lang] {
		chainAt := lexSweep(callables, scopeStart)
		for _, m := range pattern.FindAllStringSubmatchIndex(s.mask, -1) {
			chain := chainAt(m[2])
			if len(chain) == 0 {
				continue
			}
			binding := s.mask[m[2]:m[3]]
			if lang == "javascript" || lang == "typescript" {
				binding = lexBindingTargets(binding)
			}
			owner := chain[len(chain)-1]
			for _, word := range lexIdentifier.FindAllString(binding, -1) {
				owner.locals[word] = true
			}
		}
	}
}

// lexParamNames returns the names one parameter binds: the pattern before a
// type annotation or default value (Java: the last identifier).
func lexParamNames(lang, param string) []string {
	param = strings.TrimSpace(param)
	if lang == "java" {
		words := lexIdentifier.FindAllString(strings.Split(param, "=")[0], -1)
		if len(words) == 0 {
			return nil
		}
		return words[len(words)-1:]
	}
	if i := strings.IndexByte(param, '='); i >= 0 {
		param = param[:i]
	}
	if i := strings.IndexByte(param, ':'); i >= 0 && !strings.HasPrefix(strings.TrimLeft(param, "*.&"), "{") && !strings.HasPrefix(strings.TrimLeft(param, "*.&"), "[") {
		param = param[:i]
	}
	var names []string
	for _, word := range lexIdentifier.FindAllString(param, -1) {
		if word != "mut" && word != "ref" && word != "self" {
			names = append(names, word)
		}
	}
	return names
}

// lexBound reports a name bound by any function in the chain.
func lexBound(chain []*lexDecl, name string) bool {
	for _, d := range chain {
		if d.locals[name] {
			return true
		}
	}
	return false
}

// lexBindingTargets keeps the binding side of `a = 1, { b, c: d } = o`.
func lexBindingTargets(text string) string {
	var targets []string
	for _, part := range splitTopLevel(text) {
		if i := strings.IndexByte(part, '='); i >= 0 {
			part = part[:i]
		}
		targets = append(targets, part)
	}
	return strings.Join(targets, ",")
}
