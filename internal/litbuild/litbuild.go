// Package litbuild builds lit itself from this module's source, for the tests
// and tools that run a real lit: the cli suite's detached workers, and
// perfbench's measurements. Production never builds lit, and depguard keeps it
// out of production code.
package litbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Binary is a lit binary proven to work, built from the tree it was compiled
// in: it compiled, and it answered `version`.
//
// It is a type rather than a path because of what a path invites. The obvious
// way to run lit is to invoke whatever `lit` is on PATH, and that binary can be
// days behind the tree — with the staleness warning missing from the stale
// binary itself — so a test or a measurement would describe code nobody is
// looking at while appearing to describe the working tree. Only Build makes
// one, so nothing that holds a Binary can run anything else.
// [LAW:parse-dont-validate] [LAW:one-source-of-truth]
type Binary struct{ path string }

// Path is where the binary is.
func (b Binary) Path() string { return b.path }

// buildWaitDelay is how long a build that its context ended, or that exited,
// may keep its output open: the compiler's own children inherit the pipes,
// and without a bound on them the wait never returns and the context bounds
// nothing.
const buildWaitDelay = 10 * time.Second

// Build compiles ./cmd/lit into dir as dir/lit, under ctx, with the race
// detector when race is set, and proves the result runs. The compiler's output
// goes to output.
//
// The build is deliberately unstamped — no -ldflags version metadata, unlike
// `just build` and goreleaser. Claiming release provenance is goreleaser's
// alone (TestOnlyGoreleaserClaimsReleaseProvenance), and a lit that is handed
// its released-schema ceiling and raise policy, as a detached worker is by its
// spawner, needs no stamp of its own. Stamping a commit
// and a date would also make the binary depend on the checkout's index rather
// than its source.
//
// The environment is inherited untouched: the cgo ICU/zstd search paths this
// build needs live in `go env` or in what scripts/cgo-env.sh exported, the
// repository's one home for them. [LAW:single-enforcer] re-deriving them here
// would be a second copy to drift from that script.
func Build(ctx context.Context, dir string, output io.Writer, race bool) (Binary, error) {
	root, err := ModuleRoot()
	if err != nil {
		return Binary{}, err
	}
	path := filepath.Join(dir, "lit")
	args := []string{"build", "-buildvcs=false"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "-o", path, "./cmd/lit")
	// The program is named here, so what the build starts is go, and nothing a
	// caller could hand it. [LAW:types-are-the-program]
	build := exec.CommandContext(ctx, "go", args...)
	build.Dir = root
	build.Stdout, build.Stderr = output, output
	build.WaitDelay = buildWaitDelay
	if err := build.Run(); err != nil {
		return Binary{}, fmt.Errorf("go %v in %s: %w (if this is a cgo/ICU failure, run `just setup` once)", args, root, err)
	}
	// A path that compiled is not yet a binary that runs: a cgo link against
	// a library that has since moved produces exactly that, an executable that
	// exits before main.
	probe := exec.CommandContext(ctx, path, "version")
	probe.WaitDelay = buildWaitDelay
	if out, err := probe.CombinedOutput(); err != nil {
		return Binary{}, fmt.Errorf("the built lit failed `lit version`: %w\n%s", err, out)
	}
	return Binary{path: path}, nil
}

// ModuleRoot is the directory of the go.mod above this file, as it was
// compiled, so ./cmd/lit means the same package whatever the working
// directory. It is this module's one module-root finder.
// [LAW:one-source-of-truth]
func ModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", fmt.Errorf("this program does not know where its source is (%q), so it cannot find the module root", file)
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("look for go.mod in %s: %w", dir, err)
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("no go.mod above %s", file)
		}
	}
}
