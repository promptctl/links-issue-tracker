package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// The plan is only a plan if lit carries it out: every generation step must
// succeed and every probe must exit with a code that proves it did its work.
// Nothing else here runs lit, so a rule lit gains (a ticket must live inside an
// epic, say) breaks `just perf` with every other test green. This runs the
// plan through lit's own entry point at the empty control and at a size with
// more than one epic and a dependency between them. [LAW:behavior-not-structure]
func TestLitCarriesOutThePlan(t *testing.T) {
	for _, sz := range []size{{name: "empty", rows: 0}, {name: "small", rows: 25}} {
		t.Run(sz.name, func(t *testing.T) {
			state := t.TempDir()
			env := hermeticEnv(os.Environ(), filepath.Join(state, "config"), filepath.Join(state, "state"))
			bin := litBinary{path: "lit"}
			store, err := generate(bin, t.TempDir(), sz, env, inProcess(t, bin))
			if err != nil {
				t.Fatalf("generating %d rows: %v", sz.rows, err)
			}
			if len(store.ids) != sz.rows {
				t.Fatalf("generation read %d ids back from the import, want one per row (%d)", len(store.ids), sz.rows)
			}
			for _, phase := range phasesOf(invocationsFor(probes, store), invocation.probeOf) {
				for _, inv := range phase {
					code, stdout, stderr := runLitIn(t, store.root, env, gateLimits.kill(), inv.args)
					if !slices.Contains(inv.okCodes, code) {
						t.Errorf("probe %q exited %d at %d rows, want one of %v:\n%s%s",
							inv.probe.name, code, sz.rows, inv.okCodes, stdout, stderr)
					}
				}
			}
		})
	}
}

// inProcess runs a generation step through runQuiet, except that a lit step
// calls lit's entry point in this process rather than a built binary. Like
// runQuiet it answers stdout alone, which is where the import's id mapping is.
func inProcess(t *testing.T, bin litBinary) stepRunner {
	return func(dir string, env []string, st step) ([]byte, error) {
		if st.argv[0] != bin.path {
			return runQuiet(dir, env, st)
		}
		code, stdout, stderr := runLitIn(t, dir, env, st.budget, st.argv[1:])
		if code != 0 {
			return nil, fmt.Errorf("%s exited %d:\n%s%s", strings.Join(st.argv, " "), code, stdout, stderr)
		}
		return []byte(stdout), nil
	}
}

// runLitIn is one lit invocation with dir as its working directory and env
// as its environment, what a probe's subprocess would run under, answering
// the exit code cmd/lit would have exited with and the two streams apart. The
// budget is the context's
// deadline, as runQuiet's is the subprocess's, so a write wedged on the store
// lock fails this test instead of hanging it.
func runLitIn(t *testing.T, dir string, env []string, budget time.Duration, args []string) (int, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var stdout, stderr bytes.Buffer
	inv := cli.Invocation{Dir: cli.WorkingDirOf(dir, nil), Args: args, Env: environ.Of(env), Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}
	if err := cli.Run(ctx, inv); err != nil {
		return cli.WriteCommandError(&stderr, err), stdout.String(), stderr.String()
	}
	return 0, stdout.String(), stderr.String()
}
