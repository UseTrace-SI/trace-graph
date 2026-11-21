//go:build !treesitter

package graph

import (
	"regexp"
	"sort"
	"strings"
)

// Declaration-aware lexical analysis for TypeScript/JavaScript, Python, Rust
// and Java. Declarations get extents (braces or indentation over masked
// source), so a symbol's source hash covers its body and calls are
// attributed to the innermost enclosing function. Calls, imports, routes and
// type relationships are written with hints that the repository-wide pass in
// lexresolve.go resolves. A call whose name (or qualifier) is a parameter or
// local of the calling function is marked local and never resolved.

const lexExtentLimit = 1 << 20 // bytes scanned for one declaration body

type lexDecl struct {
	name, kind, container string
	signature             string
	line, endLine         int
	offset                int // start of the declaration line
	paramStart            int // offset just past the declared name
	bodyStart, bodyEnd    int // body byte range; bodyStart < 0 when bodyless
	callable, scope       bool
	static                bool // statically bound member
	supers                []lexSuper
	locals                map[string]bool
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
	rustImpl      = regexp.MustCompile(`^\s*(?:unsafe\s+)?impl\b(?:\s*<[^{]*?>)?\s+(?:([A-Za-z_][\w:]*)(?:<[^{]*?>)?\s+for\s+)?([A-Za-z_][\w:]*)`)
	heritageWord  = regexp.MustCompile(`\b(extends|implements|permits)\b`)
	pyBases       = regexp.MustCompile(`^[ \t]*class[ \t]+[A-Za-z_]\w*[ \t]*\(`)
	lexStaticWord = regexp.MustCompile(`\b(?:static|private)\b`)
	javaModifier  = wordSet("public protected private abstract static final synchronized native default strictfp transient sealed non-sealed")
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

func analyzeLexicalFile(lang, repo, rel, src string, db *indexWriter) (int, error) {
	s := newLexSource(lang, src)
	decls := lexDeclarations(lang, s)
	// Hashes keep string literals: a literal-only edit is a source change.
	hashText := maskSource(lang, src, true)
	for _, d := range decls {
		d.id = goSymbolID(rel, d.name, d.line)
		db.namespace = lexNamespace(rel, d.container, d.static)
		db.Exec("INSERT OR REPLACE INTO symbols(id,repo,file,name,kind,line,signature) VALUES(?,?,?,?,?,?,?)",
			d.id, repo, rel, d.name, d.kind, d.line, d.signature)
		db.content(repo, d.id, lexContent(s, hashText, d), d.endLine)
	}
	db.namespace = lexNamespace(rel, "", false)
	for _, d := range decls {
		for _, super := range d.supers {
			id := d.id + "->" + super.kind + ":" + super.qualifier + "." + super.name
			db.relation(repo, rel, id, d.id, d.name, super.name, super.kind, d.line, "lextype\x1f"+super.qualifier)
		}
	}
	for _, call := range lexCalls(lang, s, decls) {
		line, column := s.position(call.offset)
		db.relation(repo, rel, goRelationID(call.from.id, call.name, line, column), call.from.id, call.from.name, call.name, "call", line, call.hint)
	}
	var callables []*lexDecl
	for _, d := range decls {
		if d.callable && d.bodyStart >= 0 {
			callables = append(callables, d)
		}
	}
	writeLexRelations(lang, repo, rel, s, db, func(offset int) (string, string) {
		if from := innermost(callables, offset); from != nil {
			return from.id, from.name
		}
		return "", ""
	})
	return len(decls), nil
}

// lexNamespace identifies a symbol's file and container for the lexical
// resolver. It never contains '|', so Go resolution can never match it.
func lexNamespace(rel, container string, static bool) string {
	if static {
		return "lex:" + rel + "#" + container + "#static"
	}
	return "lex:" + rel + "#" + container
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
// body is local.
func lexDeclarations(lang string, s *lexSource) []*lexDecl {
	depth := braceDepths(s)
	var decls, scopes []*lexDecl
	seen := map[string]bool{}
	for line := 1; line <= len(s.lines); line++ {
		text := s.lineText(line)
		start := s.lines[line-1]
		if lang == "rust" {
			if m := rustImpl.FindStringSubmatch(text); m != nil {
				impl := &lexDecl{name: lastPathSegment(m[2]), kind: "impl", offset: start, line: line, scope: true}
				lexBody(lang, s, impl, start+len(m[0]))
				if m[1] != "" {
					qualifier, name := splitRustPath(m[1])
					impl.supers = append(impl.supers, lexSuper{kind: "implements", qualifier: qualifier, name: name})
				}
				scopes = append(scopes, impl)
				continue
			}
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
			scope := lexScope(lang, s, depth, scopes, start)
			if p.member && scope == nil {
				continue // a method shape outside a class body is a call
			}
			if lang == "java" && p.member && !javaMethodShape(text[:m[2]], name, scope.name) {
				break
			}
			d := &lexDecl{name: name, kind: p.kind, offset: start, paramStart: start + m[3], line: line, callable: p.callable, scope: p.scope, signature: strings.TrimSpace(text)}
			if scope != nil {
				d.container = scope.name
				switch {
				case lang == "rust":
					d.static = scope.kind == "impl"
				default:
					d.static = lexStaticWord.MatchString(text[:m[2]])
				}
			}
			if enclosing := innermost(decls, start); enclosing != nil && enclosing.callable && (scope == nil || enclosing.bodyStart > scope.bodyStart) {
				d.container, d.static = lexLocalContainer, false
			}
			if lang == "python" {
				pyBody(s, d, line)
			} else {
				lexBody(lang, s, d, start+m[3])
			}
			d.supers = lexHeritage(lang, text, m[3])
			key := d.name + "\x00" + itoa(d.line)
			if seen[key] {
				break // one identity per name and line
			}
			seen[key] = true
			decls = append(decls, d)
			if d.scope {
				scopes = append(scopes, d)
			}
			break
		}
	}
	for _, d := range decls {
		if d.callable && d.container == "" && lexReassigned(lang, s, d) {
			// A reassigned binding holds a function value: calls through it
			// are dynamic, so it is never a resolution target.
			d.container = lexLocalContainer
		}
	}
	for _, scope := range scopes {
		if scope.kind == "impl" && len(scope.supers) > 0 {
			// `impl Trait for Type` relates Type to Trait.
			for _, d := range decls {
				if d.kind == "type" && d.name == scope.name && d.container == "" {
					d.supers = append(d.supers, scope.supers...)
					break
				}
			}
		}
	}
	sort.SliceStable(decls, func(i, j int) bool { return decls[i].offset < decls[j].offset })
	return decls
}

// lexReassigned reports an assignment to a module-level callable's name
// elsewhere in the file (`let f = () => {}` later set to another function).
func lexReassigned(lang string, s *lexSource, d *lexDecl) bool {
	var pattern string
	switch lang {
	case "javascript", "typescript":
		pattern = `(?m)(?:^|[^.\w$])` + regexp.QuoteMeta(d.name) + `\s*(?:[-+*/|&]|\?\?|\|\||&&)?=(?:[^=>]|$)`
	case "python":
		pattern = `(?m)^` + regexp.QuoteMeta(d.name) + `\s*(?::[^=\n]+)?=[^=]`
	default:
		return false
	}
	for _, m := range regexp.MustCompile(pattern).FindAllStringIndex(s.mask, -1) {
		if line, _ := s.position(m[0]); line != d.line {
			return true
		}
	}
	return false
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

// lexScope returns the innermost class/impl/trait whose body directly
// contains offset (Python: whose indented block contains it).
func lexScope(lang string, s *lexSource, depth []int, scopes []*lexDecl, offset int) *lexDecl {
	var best *lexDecl
	for _, scope := range scopes {
		if scope.bodyStart < 0 || offset <= scope.bodyStart || offset >= scope.bodyEnd {
			continue
		}
		if lang != "python" && depth[offset] != depth[scope.bodyStart]+1 {
			continue // nested deeper, for example inside a method body
		}
		if best == nil || scope.bodyStart > best.bodyStart {
			best = scope
		}
	}
	return best
}

// braceDepths records the brace depth at each offset of the masked source.
func braceDepths(s *lexSource) []int {
	depth := make([]int, len(s.mask)+1)
	level := 0
	for i := 0; i < len(s.mask); i++ {
		depth[i] = level
		switch s.mask[i] {
		case '{':
			level++
		case '}':
			if level > 0 {
				level--
			}
		}
	}
	depth[len(s.mask)] = level
	return depth
}

// lexBody finds a declaration body after its name: the brace body that
// starts before a statement-ending ';', or the expression after an arrow.
func lexBody(lang string, s *lexSource, d *lexDecl, from int) {
	d.bodyStart, d.bodyEnd, d.endLine = -1, -1, d.line
	parens, arrow := 0, -1
	for i := from; i < len(s.mask) && i-from < lexExtentLimit; i++ {
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
			end := s.matching(i, lexExtentLimit)
			if end < 0 {
				return
			}
			d.bodyStart, d.bodyEnd = i, end
			d.endLine, _ = s.position(end - 1)
			return
		case c == ';' || (c == '\n' && arrow >= 0 && strings.TrimSpace(s.mask[arrow:i]) != ""):
			if arrow >= 0 {
				d.bodyStart, d.bodyEnd = arrow, i
			}
			d.endLine, _ = s.position(i)
			return
		}
	}
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
		from := innermost(callables, start)
		if from == nil || start < from.paramStart && name == from.name {
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
		if lexIsLocal(lang, s, from, local, qualifier != "") {
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
			regexp.MustCompile(`\bdef\s+\w+\s*\(([^)]*)\)`),
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

// lexIsLocal reports a name bound as a parameter or local anywhere in the
// calling function, conservatively: any binding position counts, even one in
// a nested block or closure. Java method names and variables are separate
// namespaces, so there only qualifiers are checked.
func lexIsLocal(lang string, s *lexSource, from *lexDecl, name string, qualifier bool) bool {
	if lang == "java" && !qualifier {
		return false
	}
	if from.locals == nil {
		from.locals = map[string]bool{}
		end := from.bodyEnd
		if end < from.paramStart {
			end = from.paramStart
		}
		text := s.mask[from.paramStart:end]
		if open := strings.IndexByte(text, '('); open >= 0 {
			if close := s.matching(from.paramStart+open, lexStatementLimit); close > 0 {
				for _, word := range lexIdentifier.FindAllString(s.mask[from.paramStart+open:close], -1) {
					from.locals[word] = true
				}
			}
		}
		if i := strings.Index(text, "=>"); i >= 0 && !strings.Contains(text[:i], "(") {
			for _, word := range lexIdentifier.FindAllString(text[:i], -1) {
				from.locals[word] = true
			}
		}
		for _, pattern := range lexLocalDecls[lang] {
			for _, m := range pattern.FindAllStringSubmatch(text, -1) {
				binding := m[1]
				if lang == "javascript" || lang == "typescript" {
					binding = lexBindingTargets(binding)
				}
				for _, word := range lexIdentifier.FindAllString(binding, -1) {
					from.locals[word] = true
				}
			}
		}
	}
	return from.locals[name]
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
