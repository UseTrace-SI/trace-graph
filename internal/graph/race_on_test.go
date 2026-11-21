//go:build race

package graph

// raceSlowdown scales wall-clock budgets in timing tests. The race detector
// slows this package's pure-Go work by roughly an order of magnitude (CI measured 13-14 s against 1.1 s without it),
// while the regressions these budgets guard against are slower still.
const raceSlowdown = 15
