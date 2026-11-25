package embed

import "testing"

func TestEmbedSplitsIdentifiers(t *testing.T) {
	if got := Cosine(Embed("analyze repo"), Embed(SymbolText("graph/index.go", "AnalyzeRepo", ""))); got < 0.5 {
		t.Fatalf("split identifier words do not match: %f", got)
	}
	if got := Cosine(Embed("index"), Embed(SymbolText("graph/index.go", "Unrelated", ""))); got <= 0 {
		t.Fatalf("file path words are not embedded: %f", got)
	}
	if got := Cosine(Embed("AnalyzeRepo"), Embed("analyzerepo")); got <= 0 {
		t.Fatalf("whole identifier is not embedded: %f", got)
	}
}
