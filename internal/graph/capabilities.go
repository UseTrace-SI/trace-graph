package graph

import "strings"

type Analysis struct {
	Analyzer      string   `json:"analyzer"`
	Relationships string   `json:"relationships"`
	Limitations   []string `json:"limitations"`
}

func Capabilities() Analysis {
	if strings.HasPrefix(analyzerVersion, "treesitter") {
		return Analysis{Analyzer: analyzerVersion, Relationships: "observed calls with explicit source identities; targets unresolved", Limitations: []string{"Call targets are not type-checked; dynamic dispatch remains unresolved.", "Persistent indexing follows the current checkout; explicit commit comparisons are ephemeral."}}
	}
	return Analysis{Analyzer: analyzerVersion, Relationships: "Go calls resolved by package-wide go/types checking of the module (variable and factory-return types, cross-file and import aliases, promoted methods), with syntax evidence where types are unknown; implements and interface dispatch edges; other calls unresolved", Limitations: []string{
		"Non-Go symbols use lexical inventory, not a language parser.",
		"Go type checking loads only module packages from source; standard-library and dependency types are unknown, so calls depending on them keep syntax evidence or stay unresolved.",
		"Interface and function-value calls stay unresolved; possible module implementations are separate dispatch edges.",
		"Go files are matched for the host platform without cgo; other build variants, duplicate declarations and checkouts over 32 MiB of Go keep syntax evidence.",
		"Anonymous closure bodies are omitted from enclosing-function calls.",
		"Persistent indexing follows the current checkout; explicit commit comparisons are ephemeral.",
	}}
}
