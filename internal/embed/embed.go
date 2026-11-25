package embed

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"github.com/UseTrace-SI/trace-graph/internal/ident"
)

const Dim = 128

// SymbolText is the text embedded for a symbol: its name, signature and
// repository-relative file path. Embed splits the identifiers in it.
func SymbolText(file, name, signature string) string {
	return name + " " + signature + " " + file
}

// Embed projects the words of text into a normalized hashed bag of words.
// Identifiers count as their split words (AnalyzeRepo: analyze, repo) and,
// when they split, also as the whole lowercased identifier.
func Embed(text string) []float32 {
	v := make([]float32, Dim)
	add := func(word string) {
		h := fnv.New32a()
		h.Write([]byte(strings.ToLower(word)))
		v[int(h.Sum32())%Dim] += 1
	}
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		parts := ident.Split(f)
		if len(parts) > 1 {
			add(f)
		}
		for _, part := range parts {
			add(part)
		}
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func Encode(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func Decode(b []byte, dim int) []float32 {
	if len(b) < dim*4 {
		return nil
	}
	v := make([]float32, dim)
	for i := 0; i < dim; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
