package cli

import (
	"fmt"
	"io"
)

// progressf announces a phase boundary of a long-running operation (operation
// start, remote situation resolved, download beginning) on the invocation's
// stderr, so stdout stays the machine-consumable result channel while a human
// watching a slow operation can still see which phase it is in and what lit
// decided. One emitter serves every operation — the operation name is a value
// crossing the seam, not a new printer per command. [LAW:one-type-per-behavior]
//
// Progress lines are diagnostics, never results: results (payloads, reports,
// warnings) keep their existing single channels, so nothing is double-reported.
// [LAW:single-enforcer]
func progressf(stderr io.Writer, operation string, format string, args ...any) {
	fmt.Fprintf(stderr, "lit: %s: %s\n", operation, fmt.Sprintf(format, args...))
}
