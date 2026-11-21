//go:build !treesitter

package graph

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	pathpkg "path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const goTypesEnabled = true

// The type pass stops starting packages after this much time so the index
// deadline still covers the database work that follows. Packages it did not
// reach keep their syntax evidence, which is never less conservative.
const (
	goTypeTimeBudget  = 20 * time.Second
	goDispatchPerCall = 32
	goDispatchLimit   = 100000
	goEmbeddingLimit  = 4096
)

var errGoTypeBudget = errors.New("type-check time budget exhausted")

type goTypeFile struct {
	rel  string
	file *ast.File
	test bool
}

type goTypePackage struct {
	path  string
	files []*goTypeFile
	pkg   *types.Package
	state int // 0 pending, 1 checking, 2 checked
}

// goDecl is an indexed declaration reachable from a types.Object position.
type goDecl struct {
	id, name, file string
	line           int
	test           bool
	duplicate      bool
}

type goOutcomeKind int

const (
	goUnknown  goOutcomeKind = iota // keep the syntax-evidence result
	goTarget                        // static callee declared in this module
	goExternal                      // certainly not an indexed declaration
	goDynamic                       // interface or function-value dispatch
)

type goOutcome struct {
	kind   goOutcomeKind
	target string
}

type goDynamicCall struct {
	relation, fromID, fromName, file string
	line                             int
	method                           *types.Func
	iface                            *types.Named
}

type goTypePass struct {
	ctx       context.Context
	stop      time.Time
	fset      *token.FileSet
	byPath    map[string]*goTypePackage
	packages  []*goTypePackage
	decls     map[token.Pos]*goDecl
	outcomes  map[string]goOutcome
	conflicts map[string]bool
	dynamic   []goDynamicCall
	stats     goTypeStats
}

// resolveGoTypes type-checks the analyzed module's Go packages from source
// with go/types and replaces syntax evidence where the checker is certain.
// Packages outside the module (standard library, dependencies) are never
// loaded: their types are unknown, and any call whose meaning depends on an
// unknown type keeps its syntax result instead of a guess. It also records
// implements edges between module types and dispatch edges from interface
// calls to module implementations.
func resolveGoTypes(ctx context.Context, tx *sql.Tx, repo, module string, sources []goSource, overBudget bool) (goTypeStats, error) {
	if overBudget {
		return goTypeStats{Status: "skipped", Reason: "go_source_budget"}, nil
	}
	pass := &goTypePass{
		ctx:       ctx,
		stop:      time.Now().Add(goTypeTimeBudget),
		fset:      token.NewFileSet(),
		byPath:    map[string]*goTypePackage{},
		decls:     map[token.Pos]*goDecl{},
		outcomes:  map[string]goOutcome{},
		conflicts: map[string]bool{},
		stats:     goTypeStats{Status: "complete"},
	}
	pass.load(module, sources)
	for _, pkg := range pass.packages {
		if err := pass.check(pkg); errors.Is(err, errGoTypeBudget) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return pass.stats, err
	}
	err := pass.apply(tx, repo)
	return pass.stats, err
}

func (p *goTypePass) expired() bool {
	return p.ctx.Err() != nil || time.Now().After(p.stop)
}

// goBuildContext matches files for the platform Graph runs on, without cgo,
// reading build constraints from the analyzed bytes rather than the disk.
func goBuildContext(sources map[string][]byte) *build.Context {
	ctxt := build.Default
	ctxt.GOOS, ctxt.GOARCH = runtime.GOOS, runtime.GOARCH
	ctxt.CgoEnabled = false
	ctxt.BuildTags = nil
	ctxt.JoinPath = func(elem ...string) string { return pathpkg.Join(elem...) }
	ctxt.OpenFile = func(name string) (io.ReadCloser, error) {
		src, ok := sources[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader(src)), nil
	}
	return &ctxt
}

// ignoredGoDirectory reports directories the go command never builds.
func ignoredGoDirectory(dir string) bool {
	for _, part := range strings.Split(dir, "/") {
		if part == "testdata" || strings.HasPrefix(part, "_") || (strings.HasPrefix(part, ".") && part != ".") {
			return true
		}
	}
	return false
}

func importsC(file *ast.File) bool {
	for _, spec := range file.Imports {
		if path, err := strconv.Unquote(spec.Path.Value); err == nil && path == "C" {
			return true
		}
	}
	return false
}

// load groups matching sources into packages: each directory's package with
// its in-package tests, and its external _test package. A directory whose
// files declare more than one package is left to syntax evidence.
func (p *goTypePass) load(module string, sources []goSource) {
	byName := make(map[string][]byte, len(sources))
	for _, source := range sources {
		byName[source.rel] = source.src
	}
	ctxt := goBuildContext(byName)
	type dirFiles struct {
		files []*goTypeFile
		names map[string]bool
	}
	dirs := map[string]*dirFiles{}
	for _, source := range sources {
		dir, base := pathpkg.Dir(source.rel), pathpkg.Base(source.rel)
		if ignoredGoDirectory(dir) {
			p.stats.SkippedFiles++
			continue
		}
		if match, err := ctxt.MatchFile(dir, base); err != nil || !match {
			p.stats.SkippedFiles++
			continue
		}
		file, err := parser.ParseFile(p.fset, source.rel, source.src, parser.SkipObjectResolution)
		if err != nil || importsC(file) {
			p.stats.SkippedFiles++
			continue
		}
		entry := dirs[dir]
		if entry == nil {
			entry = &dirFiles{names: map[string]bool{}}
			dirs[dir] = entry
		}
		test := strings.HasSuffix(base, "_test.go")
		entry.files = append(entry.files, &goTypeFile{rel: source.rel, file: file, test: test})
		if !test {
			entry.names[file.Name.Name] = true
		}
	}
	names := make([]string, 0, len(dirs))
	for dir := range dirs {
		names = append(names, dir)
	}
	sort.Strings(names)
	for _, dir := range names {
		entry := dirs[dir]
		primary := ""
		if len(entry.names) == 1 {
			for name := range entry.names {
				primary = name
			}
		} else if len(entry.names) == 0 {
			for _, file := range entry.files {
				if !strings.HasSuffix(file.file.Name.Name, "_test") {
					primary = file.file.Name.Name
					break
				}
			}
		}
		if primary == "" && len(entry.names) > 1 {
			p.stats.SkippedFiles += len(entry.files)
			continue
		}
		path := dir
		if module != "" {
			path = module
			if dir != "." {
				path = module + "/" + dir
			}
		}
		main := &goTypePackage{path: path}
		external := &goTypePackage{path: path + "_test"}
		for _, file := range entry.files {
			switch {
			case file.file.Name.Name == primary:
				main.files = append(main.files, file)
			case file.test && primary != "" && file.file.Name.Name == primary+"_test":
				external.files = append(external.files, file)
			case file.test && primary == "" && strings.HasSuffix(file.file.Name.Name, "_test"):
				external.files = append(external.files, file)
			default:
				p.stats.SkippedFiles++
			}
		}
		for _, pkg := range []*goTypePackage{main, external} {
			if len(pkg.files) == 0 {
				continue
			}
			p.packages = append(p.packages, pkg)
			p.index(pkg)
		}
		if len(main.files) > 0 && module != "" {
			p.byPath[path] = main
		}
	}
}

// index maps declaration name positions to persistent symbol IDs. Functions
// and methods declared more than once in one package (platform variants the
// build context did not separate) are marked so they are never targets.
func (p *goTypePass) index(pkg *goTypePackage) {
	keys := map[string][]*goDecl{}
	for _, file := range pkg.files {
		for _, decl := range file.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				line := p.fset.PositionFor(d.Pos(), false).Line
				entry := &goDecl{id: goSymbolID(file.rel, d.Name.Name, line), name: d.Name.Name, file: file.rel, line: line, test: file.test}
				p.decls[d.Name.Pos()] = entry
				key := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					key = goReceiverBase(d.Recv.List[0].Type) + "." + key
				}
				keys[key] = append(keys[key], entry)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						line := p.fset.PositionFor(ts.Pos(), false).Line
						p.decls[ts.Name.Pos()] = &goDecl{id: goSymbolID(file.rel, ts.Name.Name, line), name: ts.Name.Name, file: file.rel, line: line, test: file.test}
					}
				}
			}
		}
	}
	for _, entries := range keys {
		if len(entries) > 1 {
			for _, entry := range entries {
				entry.duplicate = true
			}
		}
	}
}

func goReceiverBase(expr ast.Expr) string {
	for depth := 0; depth < receiverEvidenceDepth; depth++ {
		switch x := expr.(type) {
		case *ast.StarExpr:
			expr = x.X
		case *ast.ParenExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		case *ast.IndexListExpr:
			expr = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
	return ""
}

// Import serves module packages from source and nothing else. A failed
// import makes go/types use an empty placeholder package whose identifiers
// have invalid types, which every classification treats as unknown.
func (p *goTypePass) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	pkg := p.byPath[path]
	if pkg == nil {
		return nil, fmt.Errorf("%s is not part of the analyzed module", path)
	}
	if err := p.check(pkg); err != nil {
		return nil, err
	}
	if pkg.pkg == nil {
		return nil, fmt.Errorf("%s could not be type-checked", path)
	}
	return pkg.pkg, nil
}

func (p *goTypePass) check(pkg *goTypePackage) (err error) {
	switch pkg.state {
	case 1:
		return fmt.Errorf("import cycle through %s", pkg.path)
	case 2:
		return nil
	}
	if p.expired() {
		p.stats.Status, p.stats.Reason = "partial", "time_budget"
		return errGoTypeBudget
	}
	pkg.state = 1
	defer func() {
		pkg.state = 2
		if recovered := recover(); recovered != nil {
			pkg.pkg = nil
			p.stats.Status, p.stats.Reason = "partial", "type_checker_panic"
			err = fmt.Errorf("type-checking %s: %v", pkg.path, recovered)
		}
	}()
	files := make([]*ast.File, len(pkg.files))
	for i, file := range pkg.files {
		files[i] = file.file
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: p, DisableUnusedImportCheck: true, Error: func(error) { p.stats.TypeErrors++ }}
	pkg.pkg, _ = conf.Check(pkg.path, p.fset, files, info)
	p.stats.Packages++
	p.stats.Files += len(files)
	p.classifyCalls(pkg, info)
	for _, file := range pkg.files {
		file.file = nil // positions and types outlive the syntax tree
	}
	return nil
}

func (p *goTypePass) classifyCalls(pkg *goTypePackage, info *types.Info) {
	for _, file := range pkg.files {
		for _, decl := range file.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fromID := goSymbolID(file.rel, fn.Name.Name, p.fset.PositionFor(fn.Pos(), false).Line)
			for _, site := range goCallSites(fn.Body) {
				at := p.fset.PositionFor(site.call.Pos(), false)
				relation := goRelationID(fromID, site.callee, at.Line, at.Column)
				outcome, dynamic := p.classify(file, info, site)
				p.record(relation, outcome)
				if dynamic != nil {
					dynamic.relation, dynamic.fromID, dynamic.fromName, dynamic.file, dynamic.line = relation, fromID, fn.Name.Name, file.rel, at.Line
					p.dynamic = append(p.dynamic, *dynamic)
				}
			}
		}
	}
}

// record keeps one outcome per relation. Chained same-name calls that start
// at one position share a relation ID; differing outcomes stay unknown.
func (p *goTypePass) record(relation string, outcome goOutcome) {
	if p.conflicts[relation] {
		return
	}
	if previous, ok := p.outcomes[relation]; ok && previous != outcome {
		delete(p.outcomes, relation)
		p.conflicts[relation] = true
		return
	}
	p.outcomes[relation] = outcome
}

func (p *goTypePass) classify(caller *goTypeFile, info *types.Info, site goCallSite) (goOutcome, *goDynamicCall) {
	switch fun := site.fun.(type) {
	case *ast.Ident:
		return p.object(caller, info.Uses[fun]), nil
	case *ast.SelectorExpr:
		if selection := info.Selections[fun]; selection != nil {
			return p.selection(caller, selection)
		}
		return p.object(caller, info.Uses[fun.Sel]), nil
	}
	return goOutcome{}, nil
}

// object classifies a called identifier that is not a method selection.
func (p *goTypePass) object(caller *goTypeFile, obj types.Object) goOutcome {
	switch o := obj.(type) {
	case *types.Builtin, *types.TypeName:
		return goOutcome{kind: goExternal} // builtin call or conversion
	case *types.Var:
		if isInvalidType(o.Type()) {
			return goOutcome{}
		}
		return goOutcome{kind: goDynamic} // function value
	case *types.Func:
		return p.target(caller, o)
	}
	return goOutcome{}
}

func (p *goTypePass) selection(caller *goTypeFile, selection *types.Selection) (goOutcome, *goDynamicCall) {
	switch selection.Kind() {
	case types.FieldVal:
		if isInvalidType(selection.Type()) {
			return goOutcome{}, nil
		}
		return goOutcome{kind: goDynamic}, nil // function-typed field
	case types.MethodVal, types.MethodExpr:
		fn, ok := selection.Obj().(*types.Func)
		if !ok {
			return goOutcome{}, nil
		}
		signature, _ := fn.Type().(*types.Signature)
		if signature == nil || signature.Recv() == nil {
			return goOutcome{}, nil
		}
		if types.IsInterface(signature.Recv().Type()) {
			return goOutcome{kind: goDynamic}, &goDynamicCall{method: fn, iface: dispatchInterface(selection, signature)}
		}
		if len(selection.Index()) > 1 && !certainPromotion(selection.Recv(), len(selection.Index())-1) {
			return goOutcome{}, nil
		}
		return p.target(caller, fn), nil
	}
	return goOutcome{}, nil
}

// dispatchInterface names the interface a dynamic call goes through: the
// static receiver type when it is a named interface, otherwise the interface
// that declares the method (for example one embedded in a struct).
func dispatchInterface(selection *types.Selection, signature *types.Signature) *types.Named {
	for _, typ := range []types.Type{selection.Recv(), signature.Recv().Type()} {
		if pointer, ok := types.Unalias(typ).(*types.Pointer); ok {
			typ = pointer.Elem()
		}
		if named, ok := types.Unalias(typ).(*types.Named); ok && types.IsInterface(named) {
			return named
		}
	}
	return nil
}

func (p *goTypePass) target(caller *goTypeFile, fn *types.Func) goOutcome {
	fn = fn.Origin()
	if fn.Pkg() == nil {
		return goOutcome{kind: goExternal}
	}
	decl := p.decls[fn.Pos()]
	// A non-test build never compiles test files, so a test-file target
	// from a non-test caller can only come from a test-only method set.
	if decl == nil || decl.duplicate || (decl.test && !caller.test) {
		return goOutcome{}
	}
	return goOutcome{kind: goTarget, target: decl.id}
}

func isInvalidType(typ types.Type) bool {
	typ = types.Unalias(typ)
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(pointer.Elem())
	}
	basic, ok := typ.(*types.Basic)
	return ok && basic.Kind() == types.Invalid
}

// certainPromotion reports whether a method promoted through depth embedded
// fields cannot be shadowed by an embedded field of unknown type: a shallower
// field from a package that was not loaded could declare the same name.
func certainPromotion(recv types.Type, depth int) bool {
	level := []types.Type{recv}
	visited := 0
	for d := 1; d < depth; d++ {
		var next []types.Type
		for _, typ := range level {
			if pointer, ok := types.Unalias(typ).(*types.Pointer); ok {
				typ = pointer.Elem()
			}
			structure, ok := typ.Underlying().(*types.Struct)
			if !ok {
				continue
			}
			for i := 0; i < structure.NumFields(); i++ {
				field := structure.Field(i)
				if !field.Embedded() {
					continue
				}
				if isInvalidType(field.Type()) {
					return false
				}
				if visited++; visited > goEmbeddingLimit {
					return false
				}
				next = append(next, field.Type())
			}
		}
		level = next
	}
	return true
}

// unknownEmbedding reports whether any embedded field reachable from typ has
// an unknown type, which leaves typ's method set uncertain.
func unknownEmbedding(typ types.Type) bool {
	level := []types.Type{typ}
	seen := map[types.Type]bool{}
	visited := 0
	for len(level) > 0 {
		var next []types.Type
		for _, current := range level {
			if pointer, ok := types.Unalias(current).(*types.Pointer); ok {
				current = pointer.Elem()
			}
			if seen[current] {
				continue
			}
			seen[current] = true
			structure, ok := current.Underlying().(*types.Struct)
			if !ok {
				continue
			}
			for i := 0; i < structure.NumFields(); i++ {
				field := structure.Field(i)
				if !field.Embedded() {
					continue
				}
				if isInvalidType(field.Type()) {
					return true
				}
				if visited++; visited > goEmbeddingLimit {
					return true
				}
				next = append(next, field.Type())
			}
		}
		level = next
	}
	return false
}

// containsInvalid reports unknown types anywhere in a method signature.
// Two unknown types compare identical, so such signatures cannot prove that
// a type satisfies an interface.
func containsInvalid(typ types.Type, depth int) bool {
	if depth > 16 {
		return true
	}
	switch t := types.Unalias(typ).(type) {
	case *types.Basic:
		return t.Kind() == types.Invalid
	case *types.Pointer:
		return containsInvalid(t.Elem(), depth+1)
	case *types.Slice:
		return containsInvalid(t.Elem(), depth+1)
	case *types.Array:
		return containsInvalid(t.Elem(), depth+1)
	case *types.Map:
		return containsInvalid(t.Key(), depth+1) || containsInvalid(t.Elem(), depth+1)
	case *types.Chan:
		return containsInvalid(t.Elem(), depth+1)
	case *types.Signature:
		for _, tuple := range []*types.Tuple{t.Params(), t.Results()} {
			for i := 0; i < tuple.Len(); i++ {
				if containsInvalid(tuple.At(i).Type(), depth+1) {
					return true
				}
			}
		}
	case *types.Named:
		args := t.TypeArgs()
		for i := 0; i < args.Len(); i++ {
			if containsInvalid(args.At(i), depth+1) {
				return true
			}
		}
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if containsInvalid(t.Field(i).Type(), depth+1) {
				return true
			}
		}
	case *types.Interface:
		for i := 0; i < t.NumEmbeddeds(); i++ {
			if containsInvalid(t.EmbeddedType(i), depth+1) {
				return true
			}
		}
		for i := 0; i < t.NumExplicitMethods(); i++ {
			if containsInvalid(t.ExplicitMethod(i).Type(), depth+1) {
				return true
			}
		}
	}
	return false
}

// certainInterface reports an ordinary interface with at least one method
// whose complete method set is known. An embedded interface of unknown type
// would silently drop its methods from the set and make Implements succeed
// for types that lack them.
func certainInterface(iface *types.Interface) bool {
	if !iface.IsMethodSet() || iface.NumMethods() == 0 {
		return false
	}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		if isInvalidType(iface.EmbeddedType(i)) {
			return false
		}
	}
	for i := 0; i < iface.NumMethods(); i++ {
		if containsInvalid(iface.Method(i).Type(), 0) {
			return false
		}
	}
	return true
}

type goImplementation struct {
	concrete, iface *types.Named
	from, to        *goDecl
}

// implementations pairs module types with the module interfaces they satisfy
// (through value or pointer receivers). Generic declarations, empty
// interfaces and anything whose method set depends on unknown types are
// left out.
func (p *goTypePass) implementations() ([]goImplementation, map[*types.Named][]*types.Named) {
	var concrete, ifaces []*types.Named
	for _, pkg := range p.packages {
		if pkg.pkg == nil {
			continue
		}
		scope := pkg.pkg.Scope()
		for _, name := range scope.Names() {
			obj, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || obj.IsAlias() || p.decls[obj.Pos()] == nil {
				continue
			}
			named, ok := obj.Type().(*types.Named)
			if !ok || named.TypeParams().Len() > 0 {
				continue
			}
			if iface, ok := named.Underlying().(*types.Interface); ok {
				if certainInterface(iface) {
					ifaces = append(ifaces, named)
				}
			} else if !isInvalidType(named.Underlying()) && !unknownEmbedding(named) {
				concrete = append(concrete, named)
			}
		}
	}
	byMethod := map[string][]*types.Named{}
	for _, iface := range ifaces {
		first := iface.Underlying().(*types.Interface).Method(0).Name()
		byMethod[first] = append(byMethod[first], iface)
	}
	var result []goImplementation
	byInterface := map[*types.Named][]*types.Named{}
	for _, typ := range concrete {
		methods := types.NewMethodSet(types.NewPointer(typ))
		from := p.decls[typ.Obj().Pos()]
		seen := map[*types.Named]bool{}
		for i := 0; i < methods.Len(); i++ {
			for _, iface := range byMethod[methods.At(i).Obj().Name()] {
				if seen[iface] {
					continue
				}
				seen[iface] = true
				if !p.satisfies(typ, iface, from) {
					continue
				}
				result = append(result, goImplementation{concrete: typ, iface: iface, from: from, to: p.decls[iface.Obj().Pos()]})
				byInterface[iface] = append(byInterface[iface], typ)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].from.id != result[j].from.id {
			return result[i].from.id < result[j].from.id
		}
		return result[i].to.id < result[j].to.id
	})
	return result, byInterface
}

func (p *goTypePass) satisfies(typ, iface *types.Named, from *goDecl) bool {
	if typ.Obj().Pkg() == nil {
		return false
	}
	underlying := iface.Underlying().(*types.Interface)
	if !types.Implements(typ, underlying) && !types.Implements(types.NewPointer(typ), underlying) {
		return false
	}
	// A test file may add the methods that make a non-test type satisfy the
	// interface; that holds only in test builds.
	for i := 0; i < underlying.NumMethods(); i++ {
		method := underlying.Method(i)
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(typ), false, method.Pkg(), method.Name())
		fn, ok := obj.(*types.Func)
		if !ok {
			return false
		}
		if decl := p.decls[fn.Origin().Pos()]; decl != nil && decl.test && !from.test {
			return false
		}
	}
	return true
}

func (p *goTypePass) apply(tx *sql.Tx, repo string) error {
	existing := map[string]bool{}
	rows, err := tx.QueryContext(p.ctx, "SELECT id FROM symbols WHERE repo=?", repo)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		existing[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	update, err := tx.PrepareContext(p.ctx, "UPDATE relation_targets SET to_id=?,resolution=? WHERE relation_id=?")
	if err != nil {
		return err
	}
	defer update.Close()
	relations := make([]string, 0, len(p.outcomes))
	for relation := range p.outcomes {
		relations = append(relations, relation)
	}
	sort.Strings(relations)
	for _, relation := range relations {
		outcome := p.outcomes[relation]
		var target any
		resolution := "unresolved"
		switch outcome.kind {
		case goTarget:
			scoped := scopedID(repo, outcome.target)
			if !existing[scoped] {
				continue
			}
			target, resolution = scoped, "typed"
		case goDynamic:
			resolution = "dynamic"
		case goExternal:
		default:
			continue
		}
		result, err := update.ExecContext(p.ctx, target, resolution, scopedID(repo, relation))
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed > 0 {
			switch outcome.kind {
			case goTarget:
				p.stats.TypedCalls++
			case goDynamic:
				p.stats.DynamicCalls++
			}
		}
	}
	insert := func(id, from, to, kind, file string, line int, fromID, toID, hint string) (bool, error) {
		if !existing[fromID] || !existing[toID] {
			return false, nil
		}
		result, err := tx.ExecContext(p.ctx, "INSERT INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", id, repo, from, to, kind, file, line)
		if err != nil {
			return false, err
		}
		if inserted, _ := result.RowsAffected(); inserted == 0 {
			return false, nil
		}
		_, err = tx.ExecContext(p.ctx, "INSERT INTO relation_targets(relation_id,from_id,to_id,namespace,hint,resolution) VALUES(?,?,?,?,?,'typed') ON CONFLICT(relation_id) DO NOTHING", id, fromID, toID, file, hint)
		return err == nil, err
	}
	pairs, byInterface := p.implementations()
	for _, pair := range pairs {
		if pair.from == nil || pair.to == nil {
			continue
		}
		ok, err := insert(scopedID(repo, pair.from.id+"->implements:"+pair.to.id), pair.from.name, pair.to.name, "implements", pair.from.file, pair.from.line, scopedID(repo, pair.from.id), scopedID(repo, pair.to.id), "implements")
		if err != nil {
			return err
		}
		if ok {
			p.stats.Implements++
		}
	}
	for _, call := range p.dynamic {
		if call.iface == nil {
			continue
		}
		implementers := byInterface[call.iface]
		if len(implementers) > goDispatchPerCall {
			p.stats.DispatchSkipped++
			continue
		}
		for _, typ := range implementers {
			if p.stats.Dispatch >= goDispatchLimit {
				p.stats.DispatchSkipped++
				break
			}
			obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(typ), false, call.method.Pkg(), call.method.Name())
			fn, ok := obj.(*types.Func)
			if !ok {
				continue
			}
			fn = fn.Origin()
			if signature, _ := fn.Type().(*types.Signature); signature == nil || signature.Recv() == nil || types.IsInterface(signature.Recv().Type()) {
				continue
			}
			decl := p.decls[fn.Pos()]
			if decl == nil || decl.duplicate {
				continue
			}
			ok, err := insert(scopedID(repo, call.relation+"=>"+decl.id), call.fromName, call.method.Name(), "dispatch", call.file, call.line, scopedID(repo, call.fromID), scopedID(repo, decl.id), "dispatch:"+call.iface.Obj().Name())
			if err != nil {
				return err
			}
			if ok {
				p.stats.Dispatch++
			}
		}
	}
	return nil
}
