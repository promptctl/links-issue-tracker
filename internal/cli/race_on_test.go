//go:build race

package cli

// raceEnabled says this test binary was built with the race detector, so the
// lit it builds for its workers is too.
const raceEnabled = true
