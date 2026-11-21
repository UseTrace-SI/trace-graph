//go:build !race

package graph

// raceSlowdown scales wall-clock budgets in timing tests; see race_on_test.go.
const raceSlowdown = 1
