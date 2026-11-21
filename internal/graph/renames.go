package graph

import (
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"math"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"
)

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

// Symbol rename evidence. A declaration's shape is computed from the bytes a
// revision analysis already holds: its comment-masked tokens with every
// occurrence of its own name replaced, so a pure rename keeps the shape, and
// a bottom-k sketch of token trigrams that estimates how similar two bodies
// are. Only analyzers that hash whole bodies take part; line inventory of
// other languages would make any two matching declaration lines "renames".
const (
	shapeSketchSize      = 32
	shapeMinTokens       = 12
	renameMinScore       = 0.7
	renameMinMargin      = 0.1
	renameMaxComparisons = 250000
)

type symbolShape struct {
	body   [32]byte
	sketch []uint64 // ascending distinct trigram hashes, at most shapeSketchSize
	tokens int
}

// renameShapeLanguage reports files whose symbols carry body extents.
func renameShapeLanguage(rel string) bool {
	return strings.EqualFold(pathpkg.Ext(rel), ".go") || relationLanguage(rel) != "" || strings.HasPrefix(analyzerVersion, "treesitter")
}

// shapeLanguage selects the comment syntax used to mask a shape's source.
func shapeLanguage(file string) string {
	if strings.EqualFold(pathpkg.Ext(file), ".go") {
		return "go"
	}
	return relationLanguage(file)
}

// splitSymbolID splits an unscoped "file:name:line" identity.
func splitSymbolID(id string) (string, string) {
	parts := strings.Split(id, ":")
	if len(parts) < 3 {
		return "", ""
	}
	return strings.Join(parts[:len(parts)-2], ":"), parts[len(parts)-2]
}

var shapeToken = regexp.MustCompile("[A-Za-z_$][\\w$]*|[0-9][\\w.]*|\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`[^`]*`|\\S")

func shapeOf(lang, name string, src []byte) symbolShape {
	tokens := shapeToken.FindAllString(maskSource(lang, string(src), true), -1)
	for i, token := range tokens {
		if token == name {
			tokens[i] = "\x00"
		}
	}
	shape := symbolShape{body: sha256.Sum256([]byte(strings.Join(tokens, "\x1f"))), tokens: len(tokens)}
	seen := map[uint64]bool{}
	for i := 0; i+2 < len(tokens); i++ {
		h := fnv.New64a()
		h.Write([]byte(tokens[i] + "\x1f" + tokens[i+1] + "\x1f" + tokens[i+2]))
		seen[h.Sum64()] = true
	}
	for value := range seen {
		shape.sketch = append(shape.sketch, value)
	}
	sort.Slice(shape.sketch, func(i, j int) bool { return shape.sketch[i] < shape.sketch[j] })
	if len(shape.sketch) > shapeSketchSize {
		shape.sketch = shape.sketch[:shapeSketchSize]
	}
	return shape
}

// similarity estimates the Jaccard similarity of two trigram sets from their
// bottom-k sketches (ascending and distinct) by merging them.
func similarity(a, b []uint64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	union, shared, i, j := 0, 0, 0, 0
	for union < shapeSketchSize && (i < len(a) || j < len(b)) {
		switch {
		case j >= len(b) || (i < len(a) && a[i] < b[j]):
			i++
		case i >= len(a) || b[j] < a[i]:
			j++
		default:
			shared++
			i++
			j++
		}
		union++
	}
	return float64(shared) / float64(union)
}

// RenameSymbol is a removed or added symbol offered for rename pairing.
type RenameSymbol struct {
	ID, File, Name, Kind string
	SignatureShape       string // signature hash with the symbol's name replaced
}

// SymbolRename pairs a removed symbol with an added one of the same kind and
// a different name.
type SymbolRename struct {
	FromID         string  `json:"-"`
	ToID           string  `json:"-"`
	Evidence       string  `json:"evidence"` // identical_body or similar_body
	Confidence     float64 `json:"confidence"`
	BodySimilarity float64 `json:"body_similarity"`
	SignatureMatch bool    `json:"signature_match"`
	Tokens         int     `json:"tokens"`
}

// SymbolRenameAmbiguity reports identical bodies that could pair in more
// than one way; those symbols keep their ordinary added/removed changes.
type SymbolRenameAmbiguity struct {
	Kind    string   `json:"kind"`
	Removed []string `json:"removed"`
	Added   []string `json:"added"`
}

// SymbolRenames pairs removed and added symbols by body evidence. An
// identical normalized body pairs only when it is unique on both sides;
// bodies under shapeMinTokens tokens pair with lower confidence. A similar
// body pairs when each side is the other's best candidate by a margin.
// similarityChecked is false when the candidate set was too large to
// compare pairwise.
func SymbolRenames(base, head RevisionStats, removed, added []RenameSymbol) (renames []SymbolRename, ambiguous []SymbolRenameAmbiguity, similarityChecked bool) {
	renames, ambiguous, similarityChecked = []SymbolRename{}, []SymbolRenameAmbiguity{}, true
	type candidate struct {
		symbol RenameSymbol
		shape  symbolShape
	}
	collect := func(symbols []RenameSymbol, shapes map[string]symbolShape) []candidate {
		var out []candidate
		for _, symbol := range symbols {
			if shape, ok := shapes[symbol.ID]; ok && shape.tokens > 0 {
				out = append(out, candidate{symbol, shape})
			}
		}
		return out
	}
	before, after := collect(removed, base.shapes), collect(added, head.shapes)
	paired := [2]map[string]bool{{}, {}} // removed, added: identities may repeat across sides
	key := func(c candidate) string { return c.symbol.Kind + "\x00" + string(c.shape.body[:]) }
	groups := map[string][2][]candidate{}
	for _, c := range before {
		g := groups[key(c)]
		g[0] = append(g[0], c)
		groups[key(c)] = g
	}
	for _, c := range after {
		g := groups[key(c)]
		g[1] = append(g[1], c)
		groups[key(c)] = g
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		if len(g[0]) == 0 || len(g[1]) == 0 {
			continue
		}
		if len(g[0]) > 1 || len(g[1]) > 1 {
			entry := SymbolRenameAmbiguity{Kind: g[0][0].symbol.Kind}
			for _, c := range g[0] {
				entry.Removed = append(entry.Removed, c.symbol.ID)
				paired[0][c.symbol.ID] = true
			}
			for _, c := range g[1] {
				entry.Added = append(entry.Added, c.symbol.ID)
				paired[1][c.symbol.ID] = true
			}
			sort.Strings(entry.Removed)
			sort.Strings(entry.Added)
			ambiguous = append(ambiguous, entry)
			continue
		}
		from, to := g[0][0], g[1][0]
		if from.symbol.Name == to.symbol.Name {
			continue
		}
		confidence := 1.0
		if from.shape.tokens < shapeMinTokens {
			confidence = 0.75
		}
		renames = append(renames, SymbolRename{FromID: from.symbol.ID, ToID: to.symbol.ID, Evidence: "identical_body", Confidence: confidence, BodySimilarity: 1, SignatureMatch: from.symbol.SignatureShape == to.symbol.SignatureShape, Tokens: from.shape.tokens})
		paired[0][from.symbol.ID], paired[1][to.symbol.ID] = true, true
	}
	// A name and kind present on both sides is the same symbol moved or
	// edited, never the old or new name of a rename.
	names := [2]map[string]bool{{}, {}}
	for _, c := range before {
		names[0][c.symbol.Kind+"\x00"+c.symbol.Name] = true
	}
	for _, c := range after {
		names[1][c.symbol.Kind+"\x00"+c.symbol.Name] = true
	}
	var left, right []candidate
	for _, c := range before {
		if !paired[0][c.symbol.ID] && !names[1][c.symbol.Kind+"\x00"+c.symbol.Name] && c.shape.tokens >= shapeMinTokens {
			left = append(left, c)
		}
	}
	for _, c := range after {
		if !paired[1][c.symbol.ID] && !names[0][c.symbol.Kind+"\x00"+c.symbol.Name] && c.shape.tokens >= shapeMinTokens {
			right = append(right, c)
		}
	}
	if len(left)*len(right) > renameMaxComparisons {
		return renames, ambiguous, false
	}
	score := func(a, b candidate) (float64, float64) {
		if a.symbol.Kind != b.symbol.Kind || a.symbol.Name == b.symbol.Name || !strings.EqualFold(pathpkg.Ext(a.symbol.File), pathpkg.Ext(b.symbol.File)) {
			return 0, 0
		}
		sim := similarity(a.shape.sketch, b.shape.sketch)
		if a.symbol.SignatureShape != b.symbol.SignatureShape {
			return sim * 0.9, sim
		}
		return sim, sim
	}
	type best struct {
		index         int
		first, second float64
	}
	bestOf := func(n int, at func(i int) (float64, int)) best {
		result := best{index: -1}
		for i := 0; i < n; i++ {
			value, index := at(i)
			if value > result.first {
				result.second, result.first, result.index = result.first, value, index
			} else if value > result.second {
				result.second = value
			}
		}
		return result
	}
	// Each score is computed once; best matches of both sides come from the
	// same matrix, so the pass costs len(left) x len(right) comparisons.
	scores := make([]float64, len(left)*len(right))
	for i := range left {
		for j := range right {
			scores[i*len(right)+j], _ = score(left[i], right[j])
		}
	}
	leftBest, rightBest := make([]best, len(left)), make([]best, len(right))
	for i := range left {
		leftBest[i] = bestOf(len(right), func(j int) (float64, int) { return scores[i*len(right)+j], j })
	}
	for j := range right {
		rightBest[j] = bestOf(len(left), func(i int) (float64, int) { return scores[i*len(right)+j], i })
	}
	for i, b := range leftBest {
		if b.index < 0 || b.first < renameMinScore || b.first-b.second < renameMinMargin {
			continue
		}
		j := b.index
		if back := rightBest[j]; back.index != i || back.first-back.second < renameMinMargin {
			continue
		}
		confidence, sim := score(left[i], right[j])
		renames = append(renames, SymbolRename{FromID: left[i].symbol.ID, ToID: right[j].symbol.ID, Evidence: "similar_body", Confidence: math.Round(confidence*100) / 100, BodySimilarity: math.Round(sim*100) / 100, SignatureMatch: left[i].symbol.SignatureShape == right[j].symbol.SignatureShape, Tokens: left[i].shape.tokens})
	}
	sort.Slice(renames, func(i, j int) bool { return renames[i].FromID < renames[j].FromID })
	return renames, ambiguous, true
}

// SignatureShape hashes a signature with the symbol's own name replaced, so
// a renamed symbol with an unchanged signature keeps the same shape.
func SignatureShape(name, signature string) string {
	tokens := shapeToken.FindAllString(signature, -1)
	for i, token := range tokens {
		if token == name {
			tokens[i] = "\x00"
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(tokens, "\x1f"))))
}
