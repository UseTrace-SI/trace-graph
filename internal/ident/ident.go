// Package ident splits source identifiers into the words people search for.
package ident

import (
	"strings"
	"unicode"
)

// Split breaks an identifier at separators (anything other than a letter or
// digit), lower-to-upper case changes, the last capital of an acronym that
// starts a word, and letter/digit boundaries:
//
//	AnalyzeRepoContext -> Analyze Repo Context
//	analyze_file       -> analyze file
//	HTTPServer         -> HTTP Server
//	utf8Decode         -> utf 8 Decode
//	getIDs             -> get IDs
func Split(name string) []string {
	var words []string
	runes := []rune(name)
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			words = append(words, string(runes[start:end]))
		}
		start = -1
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
			continue
		}
		prev := runes[i-1]
		switch {
		case unicode.IsDigit(r) != unicode.IsDigit(prev):
			flush(i)
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush(i)
		case unicode.IsUpper(prev) && unicode.IsUpper(r) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) && !pluralAcronym(runes, i):
			flush(i)
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(runes))
	return words
}

// pluralAcronym reports an acronym's plural "s" after runes[i] (IDs, URLs),
// which belongs to the acronym rather than starting a word.
func pluralAcronym(runes []rune, i int) bool {
	return runes[i+1] == 's' && (i+2 == len(runes) || !unicode.IsLower(runes[i+2]))
}

// Terms returns the split words of name separated by spaces, the indexed
// form of an identifier for word and prefix search.
func Terms(name string) string { return strings.Join(Split(name), " ") }
