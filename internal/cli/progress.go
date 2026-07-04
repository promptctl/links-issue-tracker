package cli

import (
	"fmt"
	"io"
	"os"
)

// progressOut is where phase-boundary progress lines are written: stderr, so
// stdout stays the machine-consumable result channel while a human watching a
// slow operation can still see which phase it is in and what lit decided.
// [LAW:effects-at-boundaries] one writer is the single seam every progress
// line crosses. A var, not a const, solely so tests can capture the lines;
// production never reassigns it.
var progressOut io.Writer = os.Stderr

// progressf announces a phase boundary of a long-running operation (operation
// start, remote situation resolved, download beginning). One emitter serves
// every operation — the operation name is a value crossing the seam, not a
// new printer per command. [LAW:one-type-per-behavior]
//
// Progress lines are diagnostics, never results: results (payloads, reports,
// warnings) keep their existing single channels, so nothing is double-reported.
// [LAW:single-enforcer]
func progressf(operation string, format string, args ...any) {
	fmt.Fprintf(progressOut, "lit: %s: %s\n", operation, fmt.Sprintf(format, args...))
}
