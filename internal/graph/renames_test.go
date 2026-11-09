package graph

import (
	"reflect"
	"testing"
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
