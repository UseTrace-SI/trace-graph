package graph

type Analysis struct {
	Analyzer      string   `json:"analyzer"`
	Relationships string   `json:"relationships"`
	Limitations   []string `json:"limitations"`
}

func Capabilities() Analysis {
	if analyzerVersion == "treesitter-v4" {
		return Analysis{Analyzer: analyzerVersion, Relationships: "observed calls with explicit source identities; targets unresolved", Limitations: []string{"Call targets are not type-checked; dynamic dispatch remains unresolved.", "Only the current checkout is indexed."}}
	}
	return Analysis{Analyzer: analyzerVersion, Relationships: "Go unique package functions, module-local import calls and explicitly declared local receivers; other calls unresolved", Limitations: []string{"Non-Go symbols use lexical inventory, not a language parser.", "Dynamic dispatch, aliases, inferred receivers and ambiguous targets remain unresolved; no type-checking is performed.", "Anonymous closure bodies are omitted from enclosing-function calls.", "Only the current checkout is indexed."}}
}
