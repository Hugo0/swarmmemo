//go:build race

package testrace

// Enabled reports whether the binary was built with the race detector, which
// makes code several times slower: timing assertions scale their bounds by
// Slowdown rather than drop them.
const Enabled = true

// Slowdown is how much slower a race build runs, as a bound multiplier.
const Slowdown = 10
