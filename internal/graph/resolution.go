package graph

// goTypeSourceBudget bounds the Go source retained for package-wide type
// checking during one index. A larger checkout keeps syntax evidence for
// every Go call rather than risking the index deadline or memory. Tests
// lower it; production never changes it.
var goTypeSourceBudget = 32 << 20

// goSource is one analyzed Go file, kept for package-wide type checking.
type goSource struct {
	rel string
	src []byte
}

// goTypeStats describes the last package-wide Go type pass. It is stored
// with the generation and reported by health.
type goTypeStats struct {
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Packages     int    `json:"packages"`
	Files        int    `json:"files"`
	SkippedFiles int    `json:"skipped_files"`
	// IncompletePackages have Go files the pass did not see; promoted
	// methods and implements edges of their types keep syntax evidence.
	IncompletePackages int `json:"incomplete_packages"`
	TypeErrors         int `json:"type_errors"`
	TypedCalls         int `json:"typed_calls"`
	DynamicCalls       int `json:"dynamic_calls"`
	Implements         int `json:"implements"`
	Dispatch           int `json:"dispatch"`
	DispatchSkipped    int `json:"dispatch_skipped"`
}
