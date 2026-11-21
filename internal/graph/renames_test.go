package graph

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRegressionExactFileRenamesRemainConservative(t *testing.T) {
	source := func(file, extension, hash string) revisionSource {
		return revisionSource{file: file, extension: extension, hash: hash}
	}
	a := source("old.go", ".go", "content")
	b := source("new.go", ".go", "content")
	retained := source("retained.go", ".go", "content")
	for _, test := range []struct {
		name        string
		base, head  []revisionSource
		baseExtra   []string
		headExtra   []string
		renames     int
		ambiguities int
	}{
		{name: "unique relocation", base: []revisionSource{a}, head: []revisionSource{b}, renames: 1},
		{name: "same path", base: []revisionSource{a}, head: []revisionSource{a}},
		{name: "copy", base: []revisionSource{a}, head: []revisionSource{a, b}},
		{name: "different bytes", base: []revisionSource{a}, head: []revisionSource{source("new.go", ".go", "modified")}},
		{name: "different extension", base: []revisionSource{a}, head: []revisionSource{source("new.js", ".js", "content")}},
		{name: "unobserved old path survives", base: []revisionSource{a}, head: []revisionSource{b}, headExtra: []string{"old.go"}},
		{name: "unobserved new path already existed", base: []revisionSource{a}, head: []revisionSource{b}, baseExtra: []string{"new.go"}},
		{name: "retained duplicate", base: []revisionSource{a, retained}, head: []revisionSource{b, retained}, ambiguities: 1},
		{name: "multiple removed", base: []revisionSource{a, retained}, head: []revisionSource{b}, ambiguities: 1},
		{name: "multiple added", base: []revisionSource{a}, head: []revisionSource{b, retained}, ambiguities: 1},
		{name: "no old analyzed source", head: []revisionSource{b}},
		{name: "no new analyzed source", base: []revisionSource{a}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stats := func(sources []revisionSource, extra []string) RevisionStats {
				result := RevisionStats{sources: sources, paths: map[string]bool{}}
				for _, source := range sources {
					result.paths[source.file] = true
				}
				for _, file := range extra {
					result.paths[file] = true
				}
				return result
			}
			base, head := stats(test.base, test.baseExtra), stats(test.head, test.headExtra)
			renames, ambiguous := ExactFileRenames(base, head)
			if renames == nil || ambiguous == nil || len(renames) != test.renames || len(ambiguous) != test.ambiguities {
				t.Fatalf("unexpected pairing: %+v %+v", renames, ambiguous)
			}
			if len(renames) == 1 && (renames[0].From != a.file || renames[0].To != b.file || renames[0].SourceHash != a.hash) {
				t.Fatalf("wrong relocation identity: %+v", renames)
			}
			if len(ambiguous) > 0 && (ambiguous[0].BaseMatches != len(test.base) || ambiguous[0].HeadMatches != len(test.head)) {
				t.Fatalf("wrong ambiguity counts: %+v", ambiguous)
			}
		})
	}
}

func TestRegressionExactFileRenameOrderIsDeterministic(t *testing.T) {
	base := RevisionStats{sources: []revisionSource{{file: "z.go", extension: ".go", hash: "a"}, {file: "a.go", extension: ".go", hash: "z"}}, paths: map[string]bool{"z.go": true, "a.go": true}}
	head := RevisionStats{sources: []revisionSource{{file: "new-z.go", extension: ".go", hash: "a"}, {file: "new-a.go", extension: ".go", hash: "z"}}, paths: map[string]bool{"new-z.go": true, "new-a.go": true}}
	renames, _ := ExactFileRenames(base, head)
	want := []FileRename{{From: "a.go", To: "new-a.go", SourceHash: "z"}, {From: "z.go", To: "new-z.go", SourceHash: "a"}}
	if !reflect.DeepEqual(renames, want) {
		t.Fatalf("unstable order: %+v", renames)
	}
}

func TestSymbolRenamesPairByNormalizedBody(t *testing.T) {
	body := func(name, extra string) string {
		return "func " + name + "(a, b int) int {\n\tsum := a + b // total\n\tif sum > 10 {\n\t\treturn " + name + "(sum-10, 0)\n\t}\n" + extra + "\treturn sum * 2\n}"
	}
	type side struct{ id, name, kind, src string }
	for _, tc := range []struct {
		test           string
		removed, added []side
		want           string
	}{
		{"identical body, recursive self-reference renamed", []side{{"b1", "Old", "func", body("Old", "")}}, []side{{"h1", "New", "func", body("New", "")}}, "b1->h1 identical_body 1"},
		{"comments and layout do not matter", []side{{"b1", "Old", "func", body("Old", "")}}, []side{{"h1", "New", "func", strings.ReplaceAll(strings.ReplaceAll(body("New", ""), "// total", "/* moved */"), "\n\t", "\n  ")}}, "b1->h1 identical_body 1"},
		{"small identical body has lower confidence", []side{{"b1", "Old", "func", "func Old() {}"}}, []side{{"h1", "New", "func", "func New() {}"}}, "b1->h1 identical_body 0.75"},
		{"duplicate identical bodies stay ambiguous", []side{{"b1", "A", "func", body("A", "")}, {"b2", "B", "func", body("B", "")}}, []side{{"h1", "C", "func", body("C", "")}, {"h2", "D", "func", body("D", "")}}, "ambiguous"},
		{"edited body pairs by similarity", []side{{"b1", "Old", "func", body("Old", "")}}, []side{{"h1", "New", "func", body("New", "\tlog(sum)\n")}}, "b1->h1 similar_body"},
		{"different bodies do not pair", []side{{"b1", "Old", "func", body("Old", "")}}, []side{{"h1", "New", "func", "func New(path string) error {\n\tfile, err := open(path)\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn file.Close()\n}"}}, ""},
		{"same name is not a rename", []side{{"b1", "Same", "func", body("Same", "")}}, []side{{"h1", "Same", "func", body("Same", "")}}, ""},
		{"different kinds do not pair", []side{{"b1", "Old", "func", body("Old", "")}}, []side{{"h1", "New", "method", body("New", "")}}, ""},
	} {
		t.Run(tc.test, func(t *testing.T) {
			base := RevisionStats{shapes: map[string]symbolShape{}}
			head := RevisionStats{shapes: map[string]symbolShape{}}
			var removed, added []RenameSymbol
			for _, s := range tc.removed {
				base.shapes[s.id] = shapeOf("go", s.name, []byte(s.src))
				removed = append(removed, RenameSymbol{ID: s.id, File: "p.go", Name: s.name, Kind: s.kind, SignatureShape: SignatureShape(s.name, "func "+s.name+"(a, b int) int")})
			}
			for _, s := range tc.added {
				head.shapes[s.id] = shapeOf("go", s.name, []byte(s.src))
				added = append(added, RenameSymbol{ID: s.id, File: "q.go", Name: s.name, Kind: s.kind, SignatureShape: SignatureShape(s.name, "func "+s.name+"(a, b int) int")})
			}
			renames, ambiguous, checked := SymbolRenames(base, head, removed, added)
			if !checked {
				t.Fatal("similarity was skipped")
			}
			var got []string
			for _, r := range renames {
				entry := fmt.Sprintf("%s->%s %s", r.FromID, r.ToID, r.Evidence)
				if r.Evidence == "identical_body" {
					entry += fmt.Sprintf(" %g", r.Confidence)
				} else if r.Confidence < 0.7 || r.Confidence >= 1 || !r.SignatureMatch {
					t.Fatalf("similar rename confidence out of range: %+v", r)
				}
				got = append(got, entry)
			}
			if len(ambiguous) > 0 {
				got = append(got, "ambiguous")
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("renames %q, want %q (%+v)", strings.Join(got, ","), tc.want, renames)
			}
		})
	}
}

// The reverse best match was recomputed for every removed candidate, so one
// added symbol against 4000 similar removed ones took 66 s.
func TestRegressionSymbolRenamesCompareEachPairOnce(t *testing.T) {
	body := "\tx := a + b\n\ty := x * 2\n\tz := y - 1\n\tw := z + x\n\tv := w * y\n\tu := v - z\n\treturn helper(u, w, v)\n"
	base := RevisionStats{shapes: map[string]symbolShape{}}
	head := RevisionStats{shapes: map[string]symbolShape{}}
	var removed, added []RenameSymbol
	for i := 0; i < 4000; i++ {
		id := fmt.Sprintf("a.go:f%d:%d", i, i+1)
		base.shapes[id] = shapeOf("go", fmt.Sprintf("f%d", i), []byte(fmt.Sprintf("func f%d(a, b int) int {\n%s\t_ = %d\n}", i, body, i)))
		removed = append(removed, RenameSymbol{ID: id, File: "a.go", Name: fmt.Sprintf("f%d", i), Kind: "func"})
	}
	head.shapes["b.go:g:1"] = shapeOf("go", "g", []byte("func g(a, b int) int {\n"+body+"}"))
	added = append(added, RenameSymbol{ID: "b.go:g:1", File: "b.go", Name: "g", Kind: "func"})
	start := time.Now()
	if _, _, checked := SymbolRenames(base, head, removed, added); !checked {
		t.Fatal("similarity skipped below the comparison bound")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("4000 x 1 candidates took %v", elapsed)
	}
}

// A symbol moved unchanged to another file keeps its name; it used to be
// left in the similarity pass and paired as a rename with a similar new one,
// or listed as a rename ambiguity with a new identical copy.
func TestRegressionMovedSymbolsAreNotRenames(t *testing.T) {
	body := "(a, b int) int {\n\tx := a + b\n\ty := x * 2\n\tz := y - 1\n\tw := z + x\n\treturn helper(z, w)\n}"
	edited := "(a, b int) int {\n\tx := a + b\n\ty := x * 2\n\tz := y - 1\n\tw := z + x\n\treturn helper(z, w+1)\n}"
	for _, tc := range []struct{ name, bar string }{{"similar new symbol", edited}, {"identical new copy", body}} {
		t.Run(tc.name, func(t *testing.T) {
			base := RevisionStats{shapes: map[string]symbolShape{"a.go:foo:3": shapeOf("go", "foo", []byte("func foo"+body))}}
			head := RevisionStats{shapes: map[string]symbolShape{
				"b.go:foo:3":  shapeOf("go", "foo", []byte("func foo"+body)),
				"b.go:bar:12": shapeOf("go", "bar", []byte("func bar"+tc.bar)),
			}}
			removed := []RenameSymbol{{ID: "a.go:foo:3", File: "a.go", Name: "foo", Kind: "func"}}
			added := []RenameSymbol{{ID: "b.go:foo:3", File: "b.go", Name: "foo", Kind: "func"}, {ID: "b.go:bar:12", File: "b.go", Name: "bar", Kind: "func"}}
			if renames, ambiguous, _ := SymbolRenames(base, head, removed, added); len(renames) != 0 || len(ambiguous) != 0 {
				t.Fatalf("moved symbol paired: renames %+v, ambiguities %+v", renames, ambiguous)
			}
		})
	}
}
