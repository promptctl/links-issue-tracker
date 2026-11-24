package main

import (
	"context"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/litbuild"
)

// litBinary is a path that has been proven to be a working lit binary built
// from the tree under measurement: it compiled, and it answered `version`.
//
// It is a distinct type rather than a string because of what the alternative
// invites. The obvious way to time lit is to invoke whatever `lit` is on PATH,
// and that binary can be days behind master — with the staleness warning
// missing from the stale binary itself — so the numbers would describe code
// nobody is looking at while appearing to describe the working tree. Making
// "the binary under measurement" a type that only build() can produce means no
// part of this tool can time anything else; this package's tests build fakes
// of it to drive the tool without a compiler.
// [LAW:parse-dont-validate] [LAW:one-source-of-truth]
type litBinary struct{ path string }

// build compiles ./cmd/lit into dir and verifies the result runs, through the
// one lit build the tests share (litbuild.Build). The build is unstamped, and
// that suits a measurement too: `lit version` is timed here as a control
// whose cost must not include resolving a version string that a real release
// build bakes in at compile time.
//
// The compiler's output goes to the injected progress stream, not straight to
// os.Stderr: run's whole signature exists so neither stream is a global, and a
// caller that passes a buffer must not find build's diagnostics on the real
// stderr instead. [LAW:effects-at-boundaries]
func build(dir string, progress io.Writer) (litBinary, error) {
	bin, err := litbuild.Build(context.Background(), dir, progress, false)
	if err != nil {
		return litBinary{}, err
	}
	return litBinary{path: bin.Path()}, nil
}
