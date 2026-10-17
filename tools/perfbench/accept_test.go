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
			adoptEnv(t, hermeticEnv(os.Environ(), filepath.Join(state, "config"),
				filepath.Join(state, "state"), filepath.Join(state, "data")))
			bin := litBinary{path: "lit"}
			store, err := generate(bin, t.TempDir(), sz, os.Environ(), inProcess(t, bin))
			if err != nil {
				t.Fatalf("generating %d rows: %v", sz.rows, err)
			}
			for _, phase := range phasesOf(probes) {
				for _, p := range phase {
					code, out := runLitIn(t, store.root, runBudget, p.args)
					if !slices.Contains(p.okCodes(sz.rows), code) {
						t.Errorf("probe %q exited %d at %d rows, want one of %v:\n%s",
							p.name, code, sz.rows, p.okCodes(sz.rows), out)
					}
				}
			}
		})
	}
}

// inProcess runs a generation step through runQuiet, except that a lit step
// calls lit's entry point in this process rather than a built binary.
func inProcess(t *testing.T, bin litBinary) stepRunner {
	return func(dir string, env []string, st step) error {
		if st.argv[0] != bin.path {
			return runQuiet(dir, env, st)
		}
		if code, out := runLitIn(t, dir, st.budget, st.argv[1:]); code != 0 {
			return fmt.Errorf("%s exited %d:\n%s", strings.Join(st.argv, " "), code, out)
		}
		return nil
	}
}

// adoptEnv makes env this process's whole environment for the test, since an
// in-process lit reads the process's own: what a probe's subprocess would run
// under, and nothing hermeticEnv strips.
func adoptEnv(t *testing.T, env []string) {
	keep := map[string]bool{}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		keep[key] = true
		t.Setenv(key, value)
	}
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); !keep[key] {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}

// runLitIn is one lit invocation with dir as its working directory, answering
// the exit code cmd/lit would have exited with. The budget is the context's
// deadline, as runQuiet's is the subprocess's, so a write wedged on the store
// lock fails this test instead of hanging it. lit resolves its workspace from
// the process's cwd, and t.Chdir is the one way to move it that restores it
// and refuses a parallel test.
func runLitIn(t *testing.T, dir string, budget time.Duration, args []string) (int, string) {
	t.Chdir(dir)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var out bytes.Buffer
	if err := cli.Run(ctx, &out, &out, args); err != nil {
		return cli.WriteCommandError(&out, err), out.String()
	}
	return 0, out.String()
}
