package graph

import (
	"context"
	"database/sql"
	pathpkg "path"
	"strconv"
	"strings"
)

// resolveLexical resolves the imports, calls, routes and type relationships
// that lexical analyzers record, using only the generation's own files and
// symbols. Module specifiers resolve to files of this repository (bare
// package names, the standard library and anything ambiguous stay
// unresolved). A call or reference resolves only to exactly one matching
// declaration: a module-level declaration of the calling file, an explicitly
// imported one, a member reached through an imported or same-file module or
// type, or a statically bound member of the caller's own class.
func resolveLexical(ctx context.Context, tx *sql.Tx, repo string) error {
	r := &lexResolver{files: map[string]bool{}, suffixes: map[string][]string{}, names: map[string]map[string][]lexSymbol{}, ids: map[string]lexSymbol{}, bindings: map[string]map[string]lexBinding{}}
	rows, err := tx.QueryContext(ctx, "SELECT path FROM file_cache_v2 WHERE repo=?", repo)
	if err != nil {
		return err
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		r.files[path] = true
		// A lookup path of one component matches by base name, a longer one
		// by its last two components; index each file under both keys.
		base := pathpkg.Base(path)
		r.suffixes[base] = append(r.suffixes[base], path)
		if key := suffixKey(path); key != base {
			r.suffixes[key] = append(r.suffixes[key], path)
		}
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT s.id,s.file,s.name,s.kind,s.line,scope.namespace FROM symbols s JOIN symbol_scopes scope ON scope.symbol_id=s.id WHERE s.repo=?`, repo)
	if err != nil {
		return err
	}
	scope := scopedID(repo, "")
	for rows.Next() {
		var sym lexSymbol
		var namespace string
		if err := rows.Scan(&sym.id, &sym.file, &sym.name, &sym.kind, &sym.line, &namespace); err != nil {
			rows.Close()
			return err
		}
		// Work with unscoped identities; outcomes are scoped once on write.
		sym.id = strings.TrimPrefix(sym.id, scope)
		if container, ok := strings.CutPrefix(namespace, "lex:"+sym.file+"#"); ok {
			sym.container, sym.static = strings.CutSuffix(container, "#static")
		} else if !strings.Contains(namespace, "|") {
			sym.container = lexLocalContainer // lexical inventory of other languages
		} else {
			continue
		}
		if r.names[sym.file] == nil {
			r.names[sym.file] = map[string][]lexSymbol{}
		}
		r.names[sym.file][sym.name] = append(r.names[sym.file][sym.name], sym)
		r.ids[sym.id] = sym
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.id,r.file,r.to_symbol,t.hint FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.repo=? AND substr(t.hint,1,3)='lex' ORDER BY r.id`, repo)
	if err != nil {
		return err
	}
	var pending []lexRelation
	for rows.Next() {
		var rel lexRelation
		if err := rows.Scan(&rel.id, &rel.file, &rel.to, &rel.hint); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, rel)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	type outcome struct {
		id, to, resolution string
		target             any
	}
	var outcomes []outcome
	// Imports first: they define the bindings calls resolve through.
	for _, rel := range pending {
		fields := strings.Split(rel.hint, "\x1f")
		if fields[0] != "leximport" || len(fields) != 6 {
			continue
		}
		lang, specifier, imported, local, kind := fields[1], fields[2], fields[3], fields[4], fields[5]
		written := imported
		if written == "" || kind == "default" {
			written = specifier
		}
		binding := r.resolveImport(lang, rel.file, specifier, imported, kind)
		if local != "" && (kind == "named" || kind == "default" || kind == "namespace" || kind == "module" || kind == "mod") {
			if r.bindings[rel.file] == nil {
				r.bindings[rel.file] = map[string]lexBinding{}
			}
			if previous, ok := r.bindings[rel.file][local]; ok && previous != binding {
				binding.conflict = true
			}
			r.bindings[rel.file][local] = binding
		}
		switch {
		case binding.symbol != "":
			outcomes = append(outcomes, outcome{rel.id, written, "lexical", scopedID(repo, binding.symbol)})
		case binding.module != "":
			outcomes = append(outcomes, outcome{rel.id, binding.module, "module", nil})
		default:
			outcomes = append(outcomes, outcome{rel.id, written, "unresolved", nil})
		}
	}
	for _, rel := range pending {
		fields := strings.Split(rel.hint, "\x1f")
		lang := relationLanguage(rel.file)
		target := ""
		switch {
		case fields[0] == "lexcall" && len(fields) == 3:
			target = r.resolveName(lang, rel.file, fields[1], rel.to, fields[2], false)
		case fields[0] == "lexroute" && len(fields) == 4:
			if line, err := strconv.Atoi(fields[3]); err == nil && line > 0 {
				target = r.symbolAt(rel.file, fields[2], line)
			} else if fields[2] != "" {
				target = r.resolveName(lang, rel.file, fields[1], fields[2], "", false)
			}
		case fields[0] == "lextype" && len(fields) == 2:
			target = r.resolveName(lang, rel.file, fields[1], rel.to, "", true)
		default:
			continue
		}
		if target != "" {
			outcomes = append(outcomes, outcome{rel.id, "", "lexical", scopedID(repo, target)})
		} else {
			outcomes = append(outcomes, outcome{rel.id, "", "unresolved", nil})
		}
	}
	update, err := tx.PrepareContext(ctx, "UPDATE relation_targets SET to_id=?,resolution=? WHERE relation_id=?")
	if err != nil {
		return err
	}
	defer update.Close()
	rename, err := tx.PrepareContext(ctx, "UPDATE relations SET to_symbol=? WHERE id=?")
	if err != nil {
		return err
	}
	defer rename.Close()
	for _, o := range outcomes {
		if _, err := update.ExecContext(ctx, o.target, o.resolution, o.id); err != nil {
			return err
		}
		if o.to != "" {
			if _, err := rename.ExecContext(ctx, o.to, o.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

type lexSymbol struct {
	id, file, name, kind, container string
	line                            int
	static                          bool
}

type lexRelation struct{ id, file, to, hint string }

// lexBinding is what a local import name refers to: a symbol, or a module
// file whose members it qualifies.
type lexBinding struct {
	symbol, module string
	member         string // a type imported from module, for Type.member calls
	conflict       bool
}

// lexResolver indexes files by their last two path components and symbols
// by file, name and identity, so each lookup is proportional to its matches.
type lexResolver struct {
	files    map[string]bool
	suffixes map[string][]string
	names    map[string]map[string][]lexSymbol
	ids      map[string]lexSymbol
	bindings map[string]map[string]lexBinding
}

func suffixKey(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// unique returns the only symbol in file named name that satisfies accept.
func (r *lexResolver) unique(file, name string, accept func(lexSymbol) bool) string {
	found := ""
	for _, sym := range r.names[file][name] {
		if accept(sym) {
			if found != "" {
				return ""
			}
			found = sym.id
		}
	}
	return found
}

func moduleLevel(sym lexSymbol) bool { return sym.container == "" }

func (r *lexResolver) symbolAt(file, name string, line int) string {
	id := goSymbolID(file, name, line)
	if _, ok := r.ids[id]; ok {
		return id
	}
	return ""
}

func (r *lexResolver) firstFile(candidates ...string) string {
	for _, candidate := range candidates {
		if r.files[candidate] {
			return candidate
		}
	}
	return ""
}

// uniqueSuffix finds the only indexed file equal to, or ending in "/"+, the
// given path.
func (r *lexResolver) uniqueSuffix(paths ...string) string {
	found := ""
	for _, path := range paths {
		for _, file := range r.suffixes[suffixKey(path)] {
			if file == path || strings.HasSuffix(file, "/"+path) {
				if found != "" && found != file {
					return ""
				}
				found = file
			}
		}
	}
	return found
}

// resolveImport maps an import to its module file and, for a named import,
// the declaration it names (or the submodule it names).
func (r *lexResolver) resolveImport(lang, from, specifier, imported, kind string) lexBinding {
	if lang == "java" {
		return r.javaImport(specifier, imported, kind)
	}
	module := r.resolveModule(lang, from, specifier, kind)
	if kind == "mod" {
		return lexBinding{module: r.rustChild(from, imported)}
	}
	if module == "" {
		return lexBinding{}
	}
	if imported == "" || kind == "default" || kind == "namespace" || kind == "module" || kind == "reexport" && imported == "" {
		return lexBinding{module: module}
	}
	if symbol := r.unique(module, imported, moduleLevel); symbol != "" {
		binding := lexBinding{symbol: symbol}
		if r.ids[symbol].kind == "type" {
			binding.module, binding.member = module, imported
		}
		return binding
	}
	// `from pkg import submodule` and `use crate::a::b` name modules.
	if sub := r.submodule(lang, module, imported); sub != "" {
		return lexBinding{module: sub}
	}
	return lexBinding{module: module}
}

func (r *lexResolver) submodule(lang, module, name string) string {
	dir := pathpkg.Dir(module)
	switch lang {
	case "python":
		if pathpkg.Base(module) == "__init__.py" {
			return r.firstFile(pathpkg.Join(dir, name+".py"), pathpkg.Join(dir, name, "__init__.py"))
		}
	case "rust":
		return r.rustChild(module, name)
	}
	return ""
}

func (r *lexResolver) resolveModule(lang, from, specifier, kind string) string {
	switch lang {
	case "javascript", "typescript":
		return r.jsModule(from, specifier)
	case "python":
		return r.pyModule(from, specifier)
	case "rust":
		if kind == "mod" {
			return ""
		}
		return r.rustModule(from, specifier)
	}
	return ""
}

var jsSourceExts = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// jsModule resolves relative specifiers, including the TypeScript convention
// of importing "./x.js" for the source "./x.ts". Bare specifiers name
// packages outside the repository.
func (r *lexResolver) jsModule(from, specifier string) string {
	if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
		return ""
	}
	base := pathpkg.Join(pathpkg.Dir(from), specifier)
	if base == ".." || strings.HasPrefix(base, "../") {
		return ""
	}
	candidates := []string{base}
	switch ext := pathpkg.Ext(base); ext {
	case ".js", ".jsx":
		candidates = append(candidates, strings.TrimSuffix(base, ext)+".ts", strings.TrimSuffix(base, ext)+".tsx")
	case ".mjs":
		candidates = append(candidates, strings.TrimSuffix(base, ext)+".mts")
	case ".cjs":
		candidates = append(candidates, strings.TrimSuffix(base, ext)+".cts")
	}
	for _, ext := range jsSourceExts {
		candidates = append(candidates, base+ext)
	}
	for _, ext := range jsSourceExts {
		candidates = append(candidates, base+"/index"+ext)
	}
	return r.firstFile(candidates...)
}

func (r *lexResolver) pyModule(from, specifier string) string {
	dots := len(specifier) - len(strings.TrimLeft(specifier, "."))
	parts := strings.FieldsFunc(specifier[dots:], func(c rune) bool { return c == '.' })
	if dots == 0 {
		if len(parts) == 0 {
			return ""
		}
		path := strings.Join(parts, "/")
		return r.uniqueSuffix(path+".py", path+"/__init__.py")
	}
	base := pathpkg.Dir(from)
	for i := 1; i < dots; i++ {
		if base == "." {
			return ""
		}
		base = pathpkg.Dir(base)
	}
	path := pathpkg.Join(append([]string{base}, parts...)...)
	if len(parts) == 0 {
		return r.firstFile(pathpkg.Join(base, "__init__.py"))
	}
	return r.firstFile(path+".py", pathpkg.Join(path, "__init__.py"))
}

// rustCrate returns the crate source directory of a file (the nearest
// ancestor holding lib.rs or main.rs) and the file's module path in it.
func (r *lexResolver) rustCrate(file string) (string, []string, bool) {
	for dir := pathpkg.Dir(file); ; dir = pathpkg.Dir(dir) {
		if r.files[pathpkg.Join(dir, "lib.rs")] || r.files[pathpkg.Join(dir, "main.rs")] {
			rel := strings.TrimPrefix(strings.TrimPrefix(file, dir), "/")
			if dir == "." {
				rel = file
			}
			rel = strings.TrimSuffix(rel, ".rs")
			var parts []string
			if rel != "lib" && rel != "main" {
				parts = strings.Split(rel, "/")
				if parts[len(parts)-1] == "mod" {
					parts = parts[:len(parts)-1]
				}
			}
			return dir, parts, true
		}
		if dir == "." || dir == "/" {
			return "", nil, false
		}
	}
}

func (r *lexResolver) rustFile(crate string, parts []string) string {
	if len(parts) == 0 {
		return r.firstFile(pathpkg.Join(crate, "lib.rs"), pathpkg.Join(crate, "main.rs"))
	}
	path := pathpkg.Join(append([]string{crate}, parts...)...)
	return r.firstFile(path+".rs", pathpkg.Join(path, "mod.rs"))
}

// rustModule resolves crate::, self:: and super:: paths. Other paths name
// external crates or are ambiguous in Rust 2018 and stay unresolved.
func (r *lexResolver) rustModule(from, specifier string) string {
	crate, current, ok := r.rustCrate(from)
	if !ok {
		return ""
	}
	segments := strings.Split(specifier, "::")
	var path []string
	switch segments[0] {
	case "crate":
		path = nil
	case "self":
		path = append([]string{}, current...)
	case "super":
		path = append([]string{}, current...)
		for len(segments) > 0 && segments[0] == "super" {
			if len(path) == 0 {
				return ""
			}
			path = path[:len(path)-1]
			segments = segments[1:]
		}
		segments = append([]string{"super"}, segments...)
	default:
		return ""
	}
	return r.rustFile(crate, append(path, segments[1:]...))
}

// rustChild resolves `mod name;` declared in module file parent.
func (r *lexResolver) rustChild(parent, name string) string {
	crate, current, ok := r.rustCrate(parent)
	if !ok {
		return ""
	}
	return r.rustFile(crate, append(append([]string{}, current...), name))
}

// javaClass resolves a qualified class name to its unique source file.
func (r *lexResolver) javaClass(qualified string) string {
	return r.uniqueSuffix(strings.ReplaceAll(qualified, ".", "/") + ".java")
}

// javaImport resolves `import a.b.C` to class C and `import static a.b.C.m`
// to C's static member m. Wildcard imports name no single declaration.
func (r *lexResolver) javaImport(specifier, imported, kind string) lexBinding {
	if kind != "named" {
		return lexBinding{}
	}
	if file := r.javaClass(specifier + "." + imported); file != "" {
		if symbol := r.unique(file, imported, func(sym lexSymbol) bool { return moduleLevel(sym) && sym.kind == "type" }); symbol != "" {
			return lexBinding{symbol: symbol, module: file, member: imported}
		}
		return lexBinding{module: file}
	}
	if file := r.javaClass(specifier); file != "" {
		class := specifier[strings.LastIndexByte(specifier, '.')+1:]
		return lexBinding{symbol: r.unique(file, imported, func(sym lexSymbol) bool { return sym.container == class && sym.static && sym.kind == "func" })}
	}
	return lexBinding{}
}

func (r *lexResolver) kindOf(id string, accept func(lexSymbol) bool) bool {
	sym, ok := r.ids[id]
	return ok && accept(sym)
}

// resolveName resolves a call, route handler or type reference, in order:
// a module-level declaration of the same file; a statically bound member of
// the caller's Java class; an imported declaration; a Java class of the same
// package. A qualified name resolves through a module binding, an imported
// or same-file type (its static members), or a Rust module path.
func (r *lexResolver) resolveName(lang, file, qualifier, name, callerContainer string, typeOnly bool) string {
	kindOK := func(sym lexSymbol) bool {
		if typeOnly {
			return sym.kind == "type"
		}
		return sym.kind == "func" || sym.kind == "type"
	}
	moduleDecl := func(sym lexSymbol) bool { return moduleLevel(sym) && kindOK(sym) }
	typeDecl := func(sym lexSymbol) bool { return moduleLevel(sym) && sym.kind == "type" }
	member := func(typ string) func(lexSymbol) bool {
		return func(sym lexSymbol) bool {
			return !typeOnly && sym.container == typ && sym.kind == "func" && (sym.static || lang == "python")
		}
	}
	sibling := func(class string) string {
		if lang != "java" {
			return ""
		}
		if candidate := pathpkg.Join(pathpkg.Dir(file), class+".java"); candidate != file {
			return r.firstFile(candidate)
		}
		return ""
	}
	if qualifier == "" {
		if id := r.unique(file, name, moduleDecl); id != "" {
			return id
		}
		if lang == "java" && callerContainer != "" && callerContainer != lexLocalContainer {
			if id := r.unique(file, name, member(callerContainer)); id != "" {
				return id
			}
		}
		if binding, ok := r.bindings[file][name]; ok && !binding.conflict && binding.symbol != "" && r.kindOf(binding.symbol, kindOK) {
			return binding.symbol
		}
		if class := sibling(name); class != "" {
			return r.unique(class, name, moduleDecl)
		}
		return ""
	}
	if lang == "rust" && (strings.Contains(qualifier, "::") || qualifier == "crate" || qualifier == "super" || qualifier == "self") {
		if module := r.rustModule(file, qualifier); module != "" {
			return r.unique(module, name, moduleDecl)
		}
		parent, typ := splitRustPath(qualifier)
		if module := r.rustModule(file, parent); module != "" && parent != "" {
			return r.unique(module, name, member(typ))
		}
		return ""
	}
	if binding, ok := r.bindings[file][qualifier]; ok {
		switch {
		case binding.conflict:
			return ""
		case binding.member != "":
			return r.unique(binding.module, name, member(binding.member))
		case binding.module != "" && binding.symbol == "":
			return r.unique(binding.module, name, moduleDecl)
		}
		return ""
	}
	if r.unique(file, qualifier, typeDecl) != "" {
		return r.unique(file, name, member(qualifier))
	}
	if class := sibling(qualifier); class != "" {
		return r.unique(class, name, member(qualifier))
	}
	return ""
}
