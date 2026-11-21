package graph

import "strings"

type Analysis struct {
	Analyzer      string   `json:"analyzer"`
	Relationships string   `json:"relationships"`
	Limitations   []string `json:"limitations"`
}

func Capabilities() Analysis {
	if strings.HasPrefix(analyzerVersion, "treesitter") {
		return Analysis{Analyzer: analyzerVersion, Relationships: "observed calls with explicit source identities, targets unresolved; TypeScript/JavaScript, Python, Rust and Java imports and HTTP routes resolved to repository module files", Limitations: []string{
			"Call targets are not type-checked; dynamic dispatch remains unresolved.",
			"Imports and routes come from the shared lexical extractor; Tree-sitter symbols are not import or handler targets, except decorated Python route functions.",
			"Persistent indexing follows the current checkout; explicit commit comparisons are ephemeral.",
		}}
	}
	return Analysis{Analyzer: analyzerVersion, Relationships: "Go calls resolved by package-wide go/types checking of the module (variable and factory-return types, cross-file and import aliases, promoted methods), with syntax evidence where types are unknown; implements and interface dispatch edges; TypeScript/JavaScript, Python, Rust and Java imports, calls, HTTP routes and extends/implements resolved across files from explicit imports and unique declarations; other calls unresolved", Limitations: []string{
		"TypeScript/JavaScript, Python, Rust and Java use declaration-aware lexical analysis of masked source, not a type checker; other non-Go languages use lexical inventory.",
		"Go type checking loads only module packages from source; standard-library and dependency types are unknown, so calls depending on them keep syntax evidence or stay unresolved.",
		"Go files are matched for the host platform without cgo; other build variants, duplicate declarations and checkouts over 32 MiB of Go keep syntax evidence.",
		"Interface, function-value and object-method calls stay unresolved (possible Go implementations are separate dispatch edges); Go closure bodies are omitted from enclosing-function calls.",
		"Lexical calls resolve only through same-file declarations, explicit relative or repository imports and static members; parameters, locals and reassigned bindings never resolve, and package names and path aliases stay unresolved.",
		"Routes are literal paths in Express/Hono, net/http/chi/gin/echo and FastAPI/Flask registrations; inline handlers resolve only when they delegate to a single call.",
		"Persistent indexing follows the current checkout; explicit commit comparisons are ephemeral.",
	}}
}
