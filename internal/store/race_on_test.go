//go:build race

package store

// underRace is whether the race detector instruments this binary. It slows
// every access several-fold, and by a different factor on every runner, so a
// wall-clock bound measured under it says nothing about the code.
const underRace = true
