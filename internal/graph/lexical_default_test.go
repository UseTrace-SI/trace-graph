//go:build !treesitter

package graph

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func lexEdge(target string) goEdge {
	if target == "" {
		return goEdge{"unresolved", ""}
	}
	return goEdge{"lexical", target}
}

// Each fixture checks every relation the analyzers record for it: calls
// resolve only through same-file declarations, explicit imports, module
// bindings and static members, never through a parameter or local of the
// caller, and imports of packages outside the repository stay unresolved.
func TestLexicalLanguagesResolveImportsCallsRoutesAndTypes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture map[string]string
		edges   map[string]map[string]goEdge
	}{
		{"typescript", lexTSFixture, map[string]map[string]goEdge{
			"import": {
				"src/app.ts->format@1":      lexEdge("src/util.ts:format:1"),
				"src/app.ts->Store@1":       lexEdge("src/util.ts:Store:4"),
				"src/app.ts->src/util.ts@2": {"module", ""},
				"src/app.ts->express@3":     lexEdge(""),
			},
			"call": {
				"handle->create@8":  lexEdge("src/util.ts:create:5"),
				"handle->helper@9":  lexEdge("src/util.ts:helper:8"),
				"handle->format@10": lexEdge("src/util.ts:format:1"),
				"handle->get@10":    lexEdge(""),
				"shadow->format@14": lexEdge(""),
				"run->handle@22":    lexEdge("src/app.ts:handle:7"),
				"format->trim@2":    lexEdge(""),
			},
			"route": {
				"GET /items/:id->handle@17": lexEdge("src/app.ts:handle:7"),
				"POST /items->(inline)@18":  lexEdge(""),
			},
			"extends":    {"Child->Base@21": lexEdge("src/app.ts:Base:20")},
			"implements": {"Child->Thing@21": lexEdge("")},
		}},
		{"python", lexPyFixture, map[string]map[string]goEdge{
			"import": {
				"pkg/api.py->FastAPI@1":       lexEdge(""),
				"pkg/api.py->User@2":          lexEdge("pkg/models.py:User:1"),
				"pkg/api.py->load@2":          lexEdge("pkg/models.py:load:9"),
				"pkg/api.py->pkg/models.py@3": {"module", ""},
				"pkg/api.py->pkg/models.py@4": {"module", ""},
			},
			"call": {
				"load->create@10":       lexEdge("pkg/models.py:create:6"),
				"get_user->load@10":     lexEdge("pkg/models.py:load:9"),
				"get_user->load@11":     lexEdge("pkg/models.py:load:9"),
				"get_user->load@12":     lexEdge("pkg/models.py:load:9"),
				"get_user->create@13":   lexEdge("pkg/models.py:create:6"),
				"local_shadow->load@16": lexEdge(""),
			},
			"route":   {"GET /users/{name}->get_user@8": lexEdge("pkg/api.py:get_user:9")},
			"extends": {"Admin->User@18": lexEdge("pkg/models.py:User:1")},
		}},
		{"rust", lexRustFixture, map[string]map[string]goEdge{
			"import": {
				"src/lib.rs->src/shapes.rs@1": {"module", ""},
				"src/lib.rs->Circle@2":        lexEdge("src/shapes.rs:Circle:1"),
				"src/lib.rs->area@2":          lexEdge("src/shapes.rs:area:15"),
			},
			"call": {
				"total->new@5":       lexEdge("src/shapes.rs:new:4"),
				"total->area@6":      lexEdge("src/shapes.rs:area:15"),
				"total->perimeter@6": lexEdge("src/shapes.rs:perimeter:17"),
			},
			"implements": {"Circle->Shape@1": lexEdge("src/shapes.rs:Shape:7")},
		}},
		{"java", lexJavaFixture, map[string]map[string]goEdge{
			"import": {
				"src/main/java/com/acme/app/Service.java->Strings@3": lexEdge("src/main/java/com/acme/util/Strings.java:Strings:3"),
				"src/main/java/com/acme/app/Service.java->trim@4":    lexEdge("src/main/java/com/acme/util/Strings.java:trim:6"),
			},
			"call": {
				"instance->trim@11": lexEdge("src/main/java/com/acme/util/Strings.java:trim:6"),
				"trim->trim@7":      lexEdge(""),
				"run->trim@8":       lexEdge("src/main/java/com/acme/util/Strings.java:trim:6"),
				"run->trim@9":       lexEdge("src/main/java/com/acme/util/Strings.java:trim:6"),
				"run->helper@10":    lexEdge("src/main/java/com/acme/app/Service.java:helper:14"),
				"run->make@11":      lexEdge("src/main/java/com/acme/app/Base.java:make:4"),
			},
			"extends":    {"Service->Base@6": lexEdge("src/main/java/com/acme/app/Base.java:Base:3")},
			"implements": {"Service->Runnable@6": lexEdge("")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, root := goTypeFixture(t, tc.fixture)
			for kind, want := range tc.edges {
				got := goEdges(t, db, root, kind)
				if len(got) != len(want) {
					t.Errorf("%s: %d edges %v, want %d", kind, len(got), got, len(want))
				}
				expectEdges(t, got, want)
			}
		})
	}
}

// Lexical symbols hash their whole body with comments masked and whitespace
// collapsed, so changes and compare see body edits but not comment edits.
// Resolution is recomputed for every file when any file changes.
func TestLexicalBodiesHashAndCrossFileResolutionFollowsEdits(t *testing.T) {
	db, root := goTypeFixture(t, lexTSFixture)
	hash := func(name string) string {
		t.Helper()
		var h string
		if err := db.QueryRow("SELECT c.source_hash FROM symbol_content c JOIN symbols s ON s.id=c.symbol_id WHERE s.repo=? AND s.name=? AND s.file='src/util.ts'", root, name).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	before := hash("format")
	edit := func(old, new string) {
		t.Helper()
		revisionTestWrite(t, root, "src/util.ts", strings.Replace(lexTSFixture["src/util.ts"], old, new, 1))
		if _, err := AnalyzeRepo(root, db); err != nil {
			t.Fatal(err)
		}
	}
	edit("return value.trim();", "return value.trim();   // a comment")
	if hash("format") != before {
		t.Fatal("comment-only edit changed the body hash")
	}
	edit("return value.trim();", "return value.toUpperCase();")
	if hash("format") == before {
		t.Fatal("body edit kept the declaration hash")
	}
	// Renaming the target in util.ts unresolves app.ts's cached call.
	edit("export function format(", "export function render(")
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{"handle->format@10": lexEdge("")})
	expectEdges(t, goEdges(t, db, root, "import"), map[string]goEdge{"src/app.ts->src/util.ts@1": {"module", ""}})
}

// A module-level binding that is reassigned holds whatever function was
// assigned last, so calls through it are dynamic and never resolve to the
// original declaration.
func TestLexicalReassignedBindingsAreNotTargets(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"ext.js": "let dispose = () => {};\nconst fixed = () => 1;\nfunction activate() {\n  dispose = () => { cleanup(); };\n}\nfunction deactivate() { dispose(); fixed(); }\n",
		"mod.py": "def handler():\n    pass\n\nhandler = wrap(handler)\n\ndef plain():\n    pass\n\ndef use():\n    handler()\n    plain()\n",
	})
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{
		"deactivate->dispose@6": lexEdge(""),
		"deactivate->fixed@6":   lexEdge("ext.js:fixed:2"),
		"use->handler@10":       lexEdge(""),
		"use->plain@11":         lexEdge("mod.py:plain:6"),
	})
}

// Comment syntax depends on the language: Python's // is floor division and
// JavaScript's # starts a private member. Treating both as comments hid the
// rest of the line, so later calls and private methods were lost. Content
// hashes keep string literals, so a literal-only edit is a source change.
func TestRegressionLexicalCommentSyntaxIsPerLanguage(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"calc.py": "def half(n):\n    return n // 2 + twice(n)\n\ndef twice(n):\n    return n * 2  # doubled\n",
		"box.js":  "class Box {\n  #count = 0;\n  #bump() { this.#count++; tick(); }\n  open() { return 'x'; }\n}\nfunction tick() {}\n",
	})
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{
		"half->twice@2": lexEdge("calc.py:twice:4"),
		"#bump->tick@3": lexEdge("box.js:tick:6"),
	})
	var before string
	if err := db.QueryRow("SELECT c.source_hash FROM symbol_content c JOIN symbols s ON s.id=c.symbol_id WHERE s.repo=? AND s.name='open'", root).Scan(&before); err != nil {
		t.Fatal(err)
	}
	revisionTestWrite(t, root, "box.js", "class Box {\n  #count = 0;\n  #bump() { this.#count++; tick(); }\n  open() { return 'y'; }\n}\nfunction tick() {}\n")
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := db.QueryRow("SELECT c.source_hash FROM symbol_content c JOIN symbols s ON s.id=c.symbol_id WHERE s.repo=? AND s.name='open'", root).Scan(&after); err != nil || after == before {
		t.Fatalf("string-literal edit kept the body hash: %v", err)
	}
}

// Declaration, call and reassignment analysis must stay linear in the file:
// a per-declaration regular expression over the whole file made a 568 KB,
// 6000-function JavaScript file exceed the one-minute index deadline.
func TestLexicalAnalysisOfLargeFilesIsLinear(t *testing.T) {
	var src strings.Builder
	const functions = 6000
	for i := 0; i < functions; i++ {
		fmt.Fprintf(&src, "export function f%d(a, b) {\n  const x = a + b;\n  helper(x);\n  return f%d(x, b);\n}\n", i, (i+1)%functions)
	}
	start := time.Now()
	s := newLexSource("javascript", src.String())
	decls := lexDeclarations("javascript", s)
	calls := lexCalls("javascript", s, decls)
	elapsed := time.Since(start)
	if len(decls) != functions || len(calls) != 2*functions {
		t.Fatalf("%d declarations and %d calls", len(decls), len(calls))
	}
	checkBudget(t, elapsed, 30*time.Second, fmt.Sprintf("lexical analysis of %d functions", functions))
}

// A single-component absolute Python module (`from tool import run`, with
// scripts/ on the import path) resolves to the only file of that name.
func TestRegressionPythonSingleComponentModuleResolves(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"scripts/tool.py":      "def run():\n    pass\n",
		"scripts/tool_test.py": "from tool import run\n\ndef test_run():\n    run()\n",
	})
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{"test_run->run@4": lexEdge("scripts/tool.py:run:1")})
	expectEdges(t, goEdges(t, db, root, "import"), map[string]goEdge{"scripts/tool_test.py->run@1": lexEdge("scripts/tool.py:run:1")})
}

// Without semicolons, a bodyless TypeScript declaration searched up to 1 MiB
// for a '{' or ';': a type alias took the next function's body as its extent
// and calls in an overloaded implementation were attributed to its first
// signature.
func TestRegressionLexicalBodylessDeclarationsKeepTheirExtent(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"a.ts": "export type Mode = 'a' | 'b'\n\nexport function parse(a: string): number\nexport function parse(a: number): number\nexport function parse(a: any): number {\n  return helper(a)\n}\n\nfunction helper(a: any) { return 1 }\n",
	})
	var end int
	if err := db.QueryRow("SELECT c.end_line FROM symbol_content c JOIN symbols s ON s.id=c.symbol_id WHERE s.repo=? AND s.name='Mode'", root).Scan(&end); err != nil || end != 1 {
		t.Fatalf("type alias extent ends at %d (%v), want 1", end, err)
	}
	var from string
	if err := db.QueryRow("SELECT t.from_id FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.repo=? AND r.kind='call' AND r.to_symbol='helper'", root).Scan(&from); err != nil || unscoped(from) != "a.ts:parse:5" {
		t.Fatalf("helper call attributed to %q (%v), want the implementation a.ts:parse:5", unscoped(from), err)
	}
}

// Body searches are bounded per declaration and brace bodies are matched
// once per file. 40000 semicolon-less type aliases took 61 s (past the index
// deadline) and 8000 nested functions 15 s.
func TestLexicalBodySearchesAreBounded(t *testing.T) {
	var aliases, nested strings.Builder
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&aliases, "export type T%d = 'a' | 'b'\n", i)
	}
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&nested, "function f%d() {\n", i)
	}
	nested.WriteString(strings.Repeat("}\n", 8000))
	for _, tc := range []struct{ lang, src string }{{"typescript", aliases.String()}, {"javascript", nested.String()}} {
		start := time.Now()
		s := newLexSource(tc.lang, tc.src)
		decls := lexDeclarations(tc.lang, s)
		lexCalls(tc.lang, s, decls)
		elapsed := time.Since(start)
		if len(decls) == 0 {
			t.Fatalf("%s: no declarations", tc.lang)
		}
		checkBudget(t, elapsed, 10*time.Second, tc.lang+" analysis")
	}
}

// Each case was a guessed edge stored as lexical. A name never resolves
// through a binding whose target is unknown or ambiguous.
func TestRegressionLexicalBindingsNeverGuess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture map[string]string
		edges   map[string]map[string]goEdge
	}{
		{"python import a.b binds the package a", map[string]string{
			"pkg/__init__.py": "def run():\n    pass\n",
			"pkg/util.py":     "def run():\n    pass\n",
			"app.py":          "import pkg.util\n\ndef main():\n    pkg.run()\n",
		}, map[string]map[string]goEdge{
			"call":   {"main->run@4": lexEdge("pkg/__init__.py:run:1")},
			"import": {"app.py->pkg/util.py@1": {"module", ""}},
		}},
		{"python name both imported and declared", map[string]string{
			"fast.py": "def parse(x):\n    return x\n",
			"mod.py":  "try:\n    from .fast import parse\nexcept ImportError:\n    def parse(x):\n        return None\n\ndef use():\n    return parse(1)\n",
		}, map[string]map[string]goEdge{"call": {"use->parse@8": lexEdge("")}}},
		{"default import members are unknown", map[string]string{
			"logger.js": "function log(msg) { console.log(msg) }\nclass Logger { log(m) { write(m) } }\nexport default new Logger()\n",
			"app.js":    "import logger from './logger'\nexport function main() {\n  logger.log('x')\n}\n",
		}, map[string]map[string]goEdge{"call": {"main->log@3": lexEdge("")}, "import": {"app.js->logger.js@1": {"module", ""}}}},
		{"module.exports members are unknown, destructured requires resolve", map[string]string{
			"util.js": "function helper() {}\nfunction other() {}\nmodule.exports = { helper, other }\n",
			"app.js":  "const util = require('./util')\nconst { helper } = require('./util')\nfunction main() {\n  util.other()\n  helper()\n}\n",
		}, map[string]map[string]goEdge{"call": {"main->other@4": lexEdge(""), "main->helper@5": lexEdge("util.js:helper:1")}}},
		{"ES imports resolve to exports only", map[string]string{
			"lib.js": "function hidden() {}\nfunction listed() {}\nexport function direct() {}\nexport { listed }\n",
			"app.js": "import { hidden, listed, direct } from './lib'\nimport * as lib from './lib'\nexport function main() {\n  hidden()\n  listed()\n  direct()\n  lib.hidden()\n  lib.listed()\n}\n",
		}, map[string]map[string]goEdge{"call": {
			"main->hidden@4": lexEdge(""), "main->listed@5": lexEdge("lib.js:listed:2"), "main->direct@6": lexEdge("lib.js:direct:3"),
			"main->hidden@7": lexEdge(""), "main->listed@8": lexEdge("lib.js:listed:2"),
		}}},
		{"route handler bound as a parameter", map[string]string{
			"routes.js": "function handler(req, res) {}\nexport function mount(app, handler) {\n  app.get('/x', handler)\n  handler()\n}\n",
		}, map[string]map[string]goEdge{"route": {"GET /x->handler@3": lexEdge("")}, "call": {"mount->handler@4": lexEdge("")}}},
		{"closures see their enclosing functions' parameters", map[string]string{
			"m.py": "def helper():\n    pass\n\ndef outer(helper):\n    def inner():\n        return helper()\n    return inner\n",
			"m.js": "function log(m) {}\nexport function process(items, log) {\n  const each = (x) => log(x)\n  items.forEach(each)\n}\n",
		}, map[string]map[string]goEdge{"call": {"inner->helper@6": lexEdge(""), "each->log@3": lexEdge("")}}},
		{"rust inline modules have their own scope", map[string]string{
			"src/lib.rs":    "pub mod config;\npub struct Config;\nimpl Config {\n    pub fn new() -> Self { Config }\n}\n",
			"src/config.rs": "pub struct Config;\nimpl Config {\n    pub fn new() -> Self { Config }\n}\n\npub fn build() -> Config { Config::new() }\n\n#[cfg(test)]\nmod tests {\n    use super::Config;\n    #[test]\n    fn builds() {\n        Config::new();\n    }\n}\n",
		}, map[string]map[string]goEdge{
			"import": {"src/config.rs->Config@10": lexEdge("")},
			"call":   {"builds->new@13": lexEdge(""), "build->new@6": lexEdge("src/config.rs:new:3")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, root := goTypeFixture(t, tc.fixture)
			for kind, want := range tc.edges {
				expectEdges(t, goEdges(t, db, root, kind), want)
			}
		})
	}
}

func TestLexicalOverloadTakesImplementationBodyOnItsLine(t *testing.T) {
	for _, tc := range []struct {
		src  string
		body bool
	}{
		{"function f$(a: string): void; function f$(a: any) { g(a) }\n", true},
		{"export function f(a: string): void; export function f(a: number): void; export function f(a: any) { g(a) }\n", true},
		{"function fx(): void; function fxy() { g() }\n", false},
		{"declare function h(): void;\n", false},
	} {
		s := newLexSource("typescript", tc.src)
		decls := lexDeclarations("typescript", s)
		if len(decls) == 0 {
			t.Fatalf("%q: no declarations", tc.src)
		}
		if got := decls[0].bodyStart >= 0; got != tc.body {
			t.Errorf("%q: body %v, want %v", tc.src, got, tc.body)
		}
	}
}

func TestLexicalOverloadRejectsNonSiblingCandidates(t *testing.T) {
	for _, src := range []string{
		"function h(a: string): void; foo(function h() { wrong() });\n",
		"function h(a: string): void; (function h() { wrong() })();\n",
		"function h(a: string): void; function outer() { function h() { wrong() } }\n",
		"function f(a: string): void; function fé() { wrong() }\n",
		"function f(a: string): void; x = function f() { wrong() }\n",
	} {
		decls := lexDeclarations("typescript", newLexSource("typescript", src))
		if len(decls) == 0 || decls[0].bodyStart >= 0 {
			t.Errorf("%q borrowed a body", src)
		}
	}
	decls := lexDeclarations("typescript", newLexSource("typescript", "export function h(a: string): void; foo(function h() {}); export default async function h(a: any) { right(a) }\n"))
	if len(decls) == 0 || decls[0].bodyStart < 0 {
		t.Fatal("sibling implementation after export default async not found")
	}
}

// A function declared after a statement that ends on its line is a
// declaration of that line; function expressions, and declarations behind
// an unclosed or closing bracket, are not.
func TestLexicalDeclarationAfterStatementOnItsLine(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"foo(function h() { wrong() }); export function h(a: string): void; export function h(a: any) { right(a) }\n", "h"},
		{"import x from './x'; export function g() { x() }\n", "g"},
		{"if (a) { b() } async function g() {}\n", "g"},
		{"foo(function h() { wrong() });\n", ""},
		{"foo(a, function h() {});\n", ""},
		{"}); function g() {}\n", ""},
		{"foo(a; function g() {})\n", ""},
		{"x = function g() {}; y()\n", ""},
	} {
		decls := lexDeclarations("typescript", newLexSource("typescript", tc.src))
		var names []string
		for _, d := range decls {
			names = append(names, d.name)
		}
		if got := strings.Join(names, " "); got != tc.want {
			t.Errorf("%q declared %q, want %q", tc.src, got, tc.want)
		}
	}
	src := "foo(function h() { wrong() }); export function h(a: string): void; export function h(a: any) { right(a) }\n"
	decls := lexDeclarations("typescript", newLexSource("typescript", src))
	if len(decls) != 1 || !decls[0].exported || decls[0].bodyStart < strings.Index(src, "any") || decls[0].bodyStart > strings.Index(src, "right") || decls[0].offset != strings.Index(src, " export") {
		t.Fatalf("declaration after a statement: %+v", decls)
	}
}

// `function` must be followed by whitespace or `*` before the declared name:
// functionCall() is a call, not a declaration of Call.
func TestLexicalFunctionKeywordNeedsSeparator(t *testing.T) {
	for src, want := range map[string]string{
		"functionCall();\n":             "",
		"a(); functionCall();\n":        "",
		"function* gen() {}\n":          "gen",
		"function*gen() {}\n":           "gen",
		"async function\tf() {}\n":      "f",
		"a(); export function g() {}\n": "g",
	} {
		var names []string
		for _, d := range lexDeclarations("javascript", newLexSource("javascript", src)) {
			names = append(names, d.name)
		}
		if got := strings.Join(names, " "); got != want {
			t.Errorf("%q declared %q, want %q", src, got, want)
		}
	}
	src := "function h(a: string): void; functionh(() => { wrong() }); function h(a: any) { right(a) }\n"
	decls := lexDeclarations("typescript", newLexSource("typescript", src))
	if len(decls) != 1 || decls[0].bodyStart < strings.Index(src, "wrong") {
		t.Fatalf("overload implementation after functionh(): %+v", decls)
	}
}
