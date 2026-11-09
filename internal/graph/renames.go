package graph

import "sort"

type revisionSource struct {
	file, extension, hash string
}

type FileRename struct {
	From       string `json:"from"`
	To         string `json:"to"`
	SourceHash string `json:"source_hash"`
}

type RenameAmbiguity struct {
	SourceHash  string   `json:"source_hash"`
	Extension   string   `json:"extension"`
	Removed     []string `json:"removed"`
	Added       []string `json:"added"`
	BaseMatches int      `json:"base_matches"`
	HeadMatches int      `json:"head_matches"`
}

// ExactFileRenames reports unique exact-content relocations, not Git history or
// author intent. Both paths must disappear from the other tree entirely. The
// whole analyzed source set participates in uniqueness, including retained
// identical copies. Excluded/non-regular sources never enter these groups.
func ExactFileRenames(base, head RevisionStats) ([]FileRename, []RenameAmbiguity) {
	group := func(sources []revisionSource) map[string][]revisionSource {
		groups := map[string][]revisionSource{}
		for _, source := range sources {
			key := source.extension + "\x00" + source.hash
			groups[key] = append(groups[key], source)
		}
		return groups
	}
	before, after := group(base.sources), group(head.sources)
	keys := make([]string, 0, len(before))
	for key := range before {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	renames, ambiguous := []FileRename{}, []RenameAmbiguity{}
	for _, key := range keys {
		a, b := before[key], after[key]
		removed, added := []string{}, []string{}
		for _, source := range a {
			if !head.paths[source.file] {
				removed = append(removed, source.file)
			}
		}
		for _, source := range b {
			if !base.paths[source.file] {
				added = append(added, source.file)
			}
		}
		if len(removed) == 0 || len(added) == 0 {
			continue
		}
		if len(a) == 1 && len(b) == 1 {
			renames = append(renames, FileRename{From: removed[0], To: added[0], SourceHash: a[0].hash})
		} else {
			sort.Strings(removed)
			sort.Strings(added)
			ambiguous = append(ambiguous, RenameAmbiguity{SourceHash: a[0].hash, Extension: a[0].extension, Removed: removed, Added: added, BaseMatches: len(a), HeadMatches: len(b)})
		}
	}
	sort.Slice(renames, func(i, j int) bool { return renames[i].From < renames[j].From })
	return renames, ambiguous
}
