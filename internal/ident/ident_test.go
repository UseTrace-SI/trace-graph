package ident

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	for name, want := range map[string][]string{
		"AnalyzeRepoContext": {"Analyze", "Repo", "Context"},
		"analyzeFile":        {"analyze", "File"},
		"analyze_file":       {"analyze", "file"},
		"HTTPServer":         {"HTTP", "Server"},
		"parseHTTP":          {"parse", "HTTP"},
		"utf8Decode":         {"utf", "8", "Decode"},
		"base64":             {"base", "64"},
		"x86_64":             {"x", "86", "64"},
		"__init__":           {"init"},
		"$scope":             {"scope"},
		"ID":                 {"ID"},
		"getIDs":             {"get", "IDs"},
		"parseURLsFast":      {"parse", "URLs", "Fast"},
		"HTTPSServer":        {"HTTPS", "Server"},
		"Ünïcode_Wörd":       {"Ünïcode", "Wörd"},
		"":                   nil,
		"_":                  nil,
	} {
		if got := Split(name); !reflect.DeepEqual(got, want) {
			t.Errorf("Split(%q) = %q, want %q", name, got, want)
		}
	}
	if got := Terms("AnalyzeRepo"); got != "Analyze Repo" {
		t.Fatalf("Terms = %q", got)
	}
}
