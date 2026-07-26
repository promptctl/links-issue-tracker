package store

import (
	"io"

	doltcli "github.com/dolthub/dolt/go/cmd/dolt/cli"
)

// The embedded Dolt engine narrates its own human progress — the "N of M chunks
// complete" redraw (with cursor-control escapes) that DOLT_CLONE and DOLT_FETCH
// emit during init adopt and sync pull/fetch. Dolt writes that narration to its
// package-global cli.CliOut, which defaults to os.Stdout — lit's PARSEABLE result
// channel (init reports, "pulled", payloads). The escapes therefore land in the
// one stream a caller reads for results.
//
// [LAW:effects-at-boundaries] (CLI binding: stdout is parseable, stderr is human)
// forbids that: dolt's progress is human output and must not touch stdout.
//
// We SUPPRESS it rather than relocate it to stderr because lit already owns the
// single progress voice — progressf (internal/cli/progress.go) narrates each
// phase boundary to stderr. Routing dolt's redraw to stderr too would put two
// progress narrators on one human channel, a divergent second telling of "how far
// along are we." [LAW:single-enforcer] keeps lit's narration the only one; dolt's
// redundant chatter goes to io.Discard.
//
// [LAW:no-shared-mutable-globals] cli.CliOut is dolt's process-wide global; this
// init is its single owner and sets it exactly once, before the store package can
// run any dolt operation. Suppressing CliOut (dolt's human channel) is complete:
// lit reads every dolt RESULT through SQL rows, never through CliOut, so nothing
// lit depends on is discarded. Dolt's error channel (cli.CliErr) is left untouched.
func init() {
	doltcli.CliOut = io.Discard
}
