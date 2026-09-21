package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// litBinary is a path that has been proven to be a working lit binary built
// from the tree under measurement: it compiled, and it answered `version`.
//
// It is a distinct type rather than a string because of what the alternative
// invites. The obvious way to time lit is to invoke whatever `lit` is on PATH,
// and on this machine that binary was nine days behind master when this tool
// was written — with the staleness warning missing from the stale binary
// itself, so the numbers would have described code nobody was looking at while
// appearing to describe the working tree. Making "the binary under measurement"
// a type that only build() can produce means no part of this tool can time
// anything else. [LAW:parse-dont-validate] [LAW:one-source-of-truth]
type litBinary struct{ path string }

// build compiles ./cmd/lit into dir and verifies the result runs.
//
// The build is deliberately unstamped — no -ldflags version metadata, unlike
// `just build`. Stamping shells out to git for a commit and a date, which would
// make this tool's output depend on the state of the checkout's index rather
// than on its source, and `lit version` is timed here as a control whose cost
// must not include resolving a version string that a real release build bakes
// in at compile time.
func build(dir string) (litBinary, error) {
	path := filepath.Join(dir, "lit")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", path, "./cmd/lit")
	// Inherited, not cleared: the cgo ICU/zstd search paths this build needs
	// live in `go env` or in the environment `just perf` sourced from
	// scripts/cgo-env.sh, which is the repository's one home for them.
	// [LAW:single-enforcer] re-deriving them here would be a second copy to
	// drift from that script.
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		return litBinary{}, fmt.Errorf("building ./cmd/lit: %w (if this is a cgo/ICU failure, run `just setup` once)", err)
	}
	// The stamp: a path that compiled is not yet a binary that runs — a cgo
	// link against a library that has since moved produces exactly that, an
	// executable that exits before main.
	if out, err := exec.Command(path, "version").CombinedOutput(); err != nil {
		return litBinary{}, fmt.Errorf("built binary failed `lit version`: %w\n%s", err, out)
	}
	return litBinary{path: path}, nil
}
