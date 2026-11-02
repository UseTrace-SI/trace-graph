package store

import (
	"fmt"
	"strings"
	"unicode"
)

// LiteralQuery treats task text as words rather than FTS operators.
func LiteralQuery(text string) (string, error) {
	if len(text) > 4096 {
		return "", fmt.Errorf("query exceeds 4096 bytes")
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' })
	if len(words) > 16 {
		words = words[:16]
	}
	for i := range words {
		words[i] = `"` + words[i] + `"`
	}
	if len(words) == 0 {
		return `""`, nil
	}
	return strings.Join(words, " OR "), nil
}
