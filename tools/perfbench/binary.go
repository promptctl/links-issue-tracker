package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// part of this tool can time anything else.
// [LAW:parse-dont-validate] [LAW:one-source-of-truth]
type litBinary struct{ path string }

// build compiles ./cmd/lit into dir and verifies the result runs.
//
// The build is deliberately unstamped — no -ldflags version metadata, unlike
// `just build`. Stamping shells out to git for a commit and a date, which would
// make this tool's output depend on the state of the checkout's index rather
// than on its source, and `lit version` is timed here as a control whose cost
// must not include resolving a version string that a real release build bakes
// in at compile time.
func build(dir string, progress io.Writer) (litBinary, error) {
	root, err := moduleRoot()
	if err != nil {
		return litBinary{}, err
	}
	path := filepath.Join(dir, "lit")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", path, "./cmd/lit")
	cmd.Dir = root
	// The environment is inherited untouched, unlike the probes': the cgo
	// ICU/zstd search paths this build needs live in `go env` or in what `just
	// perf` sourced from scripts/cgo-env.sh, the repository's one home for
	// them. [LAW:single-enforcer] re-deriving them here would be a second copy
	// to drift from that script.
	//
	// The compiler's output goes to the injected progress stream, not straight
	// to os.Stderr: run's whole signature exists so neither stream is a global,
	// and a caller that passes a buffer must not find build's diagnostics on
	// the real stderr instead. Both streams go to progress because neither is
	// the report. [LAW:effects-at-boundaries]
	cmd.Stderr = progress
	cmd.Stdout = progress
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

// moduleRoot is the directory holding this module's go.mod, so `./cmd/lit`
// means the same package from any working directory. Resolved rather than
// assumed to be the cwd: `go run ../tools/perfbench` from a subdirectory would
// otherwise fail on a missing ./cmd/lit and blame the cgo toolchain for it.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("locating go.mod via `go env GOMOD`: %w", err)
	}
	goMod := strings.TrimSpace(string(out))
	// Outside a module `go env GOMOD` prints os.DevNull or nothing, and there
	// is no ./cmd/lit to build. [LAW:no-silent-failure]
	if goMod == "" || goMod == os.DevNull {
		return "", fmt.Errorf("not inside a Go module (go env GOMOD = %q); run perfbench from the lit repository", goMod)
	}
	return filepath.Dir(goMod), nil
}
