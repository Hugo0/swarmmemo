//go:build !race

// Package testrace tells tests whether the race detector is on, so timing
// bounds can scale with it instead of failing or being dropped under -race.
package testrace

// Enabled reports whether the binary was built with the race detector.
const Enabled = false

// Slowdown is how much slower a race build runs, as a bound multiplier.
const Slowdown = 1
