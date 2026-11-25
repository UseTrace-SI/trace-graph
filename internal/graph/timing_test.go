package graph

import (
	"testing"
	"time"
)

// checkBudget fails when elapsed exceeds a wall-clock budget. The race
// detector slows pure-Go work by a large and machine-dependent factor, so a
// scaled budget either flakes on a slow runner or stops guarding anything;
// absolute budgets are enforced only without it. Functional assertions of
// the same tests still run under -race.
func checkBudget(t *testing.T, elapsed, budget time.Duration, what string) {
	t.Helper()
	if raceEnabled {
		t.Logf("%s took %v; the %v budget is not enforced under -race", what, elapsed, budget)
		return
	}
	if elapsed > budget {
		t.Fatalf("%s took %v, over the %v budget", what, elapsed, budget)
	}
}
