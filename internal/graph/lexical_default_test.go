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
	if elapsed > 30*time.Second {
		t.Fatalf("lexical analysis of %d functions took %v", functions, elapsed)
	}
	t.Logf("%d functions analyzed in %v", functions, elapsed)
}
