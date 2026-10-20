package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
)

// injectFailpoint fails each named failpoint with err until the returned
// clear runs or the test ends. Serial tests only: failpointForTest is
// package-level.
func injectFailpoint(t *testing.T, err error, at ...failpoint) (clear func()) {
	t.Helper()
	failpointForTest = func(got failpoint) error {
		for _, want := range at {
			if got == want {
				return err
			}
		}
		return nil
	}
	clear = func() { failpointForTest = nil }
	t.Cleanup(clear)
	return clear
}

// asConvergenceError is err's SchemaConvergenceError, failing the test when
// it has none.
func asConvergenceError(t *testing.T, err error) *SchemaConvergenceError {
	t.Helper()
	var fail *SchemaConvergenceError
	if !errors.As(err, &fail) {
		t.Fatalf("error = %v (%T); want a *SchemaConvergenceError", err, err)
	}
	return fail
}

// Every step has a token, an act, and a token no other step has, so an event
// that names a step names exactly one.
func TestEveryConvergenceStepHasATokenAndAnAct(t *testing.T) {
	seen := map[string]ConvergenceStep{}
	for _, step := range ConvergenceSteps() {
		token := step.String()
		if token == "" {
			t.Errorf("step %d has no token", int(step))
		}
		if step.Act() == 0 {
			t.Errorf("step %q has no act", token)
		}
		if other, dup := seen[token]; dup {
			t.Errorf("steps %d and %d share token %q", int(other), int(step), token)
		}
		seen[token] = step
	}
}

// The message states the snapshot and a prune failure as facts and never
// instructs a restore: whether to restore is the act's to say, and only one
// act says it.
func TestAConvergenceFailureNamesItsSnapshotWithoutAdvisingARestore(t *testing.T) {
	snap := &dbsnapshot.Snapshot{Name: "1-pre-migrate-1", Path: "/snapshots/1-pre-migrate-1"}
	for _, step := range ConvergenceSteps() {
		msg := (&SchemaConvergenceError{Step: step, Snapshot: snap, PruneErr: errors.New("prune stuck"), Cause: errors.New("synthetic")}).Error()
		for _, want := range []string{"step " + step.String() + " failed: synthetic", snap.Name, snap.Path, "prune stuck"} {
			if !strings.Contains(msg, want) {
				t.Errorf("step %q message lacks %q: %s", step, want, msg)
			}
		}
		if strings.Contains(msg, "lit snapshots restore") {
			t.Errorf("step %q message advises a restore: %s", step, msg)
		}
	}
}
