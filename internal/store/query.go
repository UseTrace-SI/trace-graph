package store

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/UseTrace-SI/trace-graph/internal/ident"
)

// queryWords extracts at most sixteen literal words (letters, digits and
// underscores) from task text.
func queryWords(text string) ([]string, error) {
	if len(text) > 4096 {
		return nil, fmt.Errorf("query exceeds 4096 bytes")
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
	if len(words) > 16 {
		words = words[:16]
	}
	return words, nil
}

// LiteralQuery treats task text as words rather than FTS operators.
func LiteralQuery(text string) (string, error) {
	words, err := queryWords(text)
	if err != nil {
		return "", err
	}
	for i := range words {
		words[i] = `"` + words[i] + `"`
	}
	if len(words) == 0 {
		return `""`, nil
	}
	return strings.Join(words, " OR "), nil
}

// SymbolSearch is a ranked symbol search. Callers join symbols s with
// symbol_fts, filter with "symbol_fts MATCH ?" bound to Match, and order by
// OrderBy followed by OrderArgs:
//
//	SELECT ... FROM symbols s JOIN symbol_fts ON symbol_fts.rowid=s.rowid
//	WHERE symbol_fts MATCH ? AND s.repo=? ORDER BY <OrderBy> LIMIT ?
type SymbolSearch struct {
	Match     string
	OrderBy   string
	OrderArgs []any
}

// NewSymbolSearch interprets text as literal words. Each word matches a
// symbol whose name or split identifier words (AnalyzeRepo -> Analyze Repo,
// analyze_file -> analyze file) start with it, or whose name, words or
// signature contain it. A camelCase or snake_case word also matches the same
// word sequence in other spellings. Results rank an exact (case-insensitive)
// name first, then names starting with a word, then by BM25 with names
// weighted over split words over signatures. schema is the index's
// user_version: indexes older than schema 4 have no split words and fall back
// to name and signature prefixes. ok is false when text has no words.
func NewSymbolSearch(text string, schema int) (search SymbolSearch, ok bool, err error) {
	words, err := queryWords(text)
	if err != nil {
		return SymbolSearch{}, false, err
	}
	var clauses, exact, prefix []string
	var exactArgs, prefixArgs []any
	for _, word := range words {
		parts := ident.Split(word)
		if len(parts) == 0 {
			continue // "_" alone has no searchable token
		}
		quoted := `"` + word + `"`
		group := []string{quoted}
		if schema >= 4 {
			group = append(group, "{name terms} : "+quoted+"*")
			if len(parts) > 1 {
				group = append(group, `{terms} : "`+strings.Join(parts, " ")+`"*`)
			}
		} else {
			group = append(group, quoted+"*")
		}
		clauses = append(clauses, "("+strings.Join(group, " OR ")+")")
		exact = append(exact, "lower(?)")
		exactArgs = append(exactArgs, word)
		prefix = append(prefix, `s.name LIKE ? ESCAPE '\'`)
		prefixArgs = append(prefixArgs, strings.ReplaceAll(word, "_", `\_`)+"%")
	}
	if len(clauses) == 0 {
		return SymbolSearch{}, false, nil
	}
	if len(words) > 1 {
		// "analyze repo" names AnalyzeRepo exactly.
		exact = append(exact, "lower(?)")
		exactArgs = append(exactArgs, strings.Join(words, ""))
	}
	weights := "10.0, 5.0, 1.0"
	if schema < 4 {
		weights = "10.0, 1.0"
	}
	search = SymbolSearch{
		Match: strings.Join(clauses, " OR "),
		OrderBy: "CASE WHEN lower(s.name) IN (" + strings.Join(exact, ",") + ") THEN 0 WHEN " + strings.Join(prefix, " OR ") +
			" THEN 1 ELSE 2 END, bm25(symbol_fts, " + weights + "), s.file, s.line, s.id",
		OrderArgs: append(exactArgs, prefixArgs...),
	}
	return search, true, nil
}
