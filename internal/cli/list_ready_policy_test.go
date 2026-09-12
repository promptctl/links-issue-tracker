package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// `lit ls` reads the repo's ready policy only for a projection that names a
// readiness-sourced column. That is not a performance note about one file read:
// config.Load validates the WHOLE config — snapshot.retention_budget,
// sync.cadence, claims.freshness_window — and returns an error for any of them,
// so reading it unconditionally made `lit ls` fail on defects in settings it
// does not render. A repo with a bad sync.cadence could not list its own
// tickets, which is precisely when someone needs to.
//
// The guarantee is invisible from inside — "we did not call config.Load" is not
// something the output shows — so it is pinned from outside, by breaking the
// config and reading which invocations still work.
func TestListReadsReadyPolicyOnlyWhenProjected(t *testing.T) {
	h := newReadyTestHarness(t)

	// Invalid for a reason that has nothing to do with listing, and not the
	// required-fields key this path consumes: a test that broke [ready] itself
	// could pass while `lit ls` still read config for every projection.
	h.writeProjectConfig("[sync]\ncadence = \"not-a-cadence\"\n")

	issue := h.createIssue(storage.CreateIssueInput{
		Prefix: "policy", Title: "Listable", Topic: "policy", IssueType: "task", Description: "d",
	})

	runLS := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := runListWithStore(h.ctx, &out, h.ap.Store, workspaceReadyPolicy(h.ap), args)
		return out.String(), err
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"default projection", nil},
		{"issue fields only", []string{"--columns", "id,title"}},
		{"the relation rung", []string{"--columns", "id,parent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runLS(tc.args...)
			if err != nil {
				t.Fatalf("lit ls %v failed against an invalid config it never needed to read: %v",
					tc.args, err)
			}
			// Erroring is not the only way to fail this: a path that swallowed
			// the config error and printed nothing would pass on err alone.
			if !strings.Contains(out, issue.ID) {
				t.Errorf("lit ls %v exited clean but did not list %s; output:\n%s",
					tc.args, issue.ID, out)
			}
		})
	}

	// The other half of the rule, and the reason this is a boundary rather than
	// a config read that was simply deleted: the rung that CONSUMES the policy
	// still surfaces the failure. [LAW:no-silent-failure] Were this to start
	// passing, `--columns blocked` would be computing readiness off a policy it
	// silently failed to load, and every cell it printed would be unsound.
	t.Run("the readiness rung still fails loudly", func(t *testing.T) {
		out, err := runLS("--columns", "id,blocked")
		if err == nil {
			t.Fatalf("lit ls --columns id,blocked succeeded against an invalid config; "+
				"the blocked cell depends on that config and cannot be computed without it. "+
				"output:\n%s", out)
		}
		if !strings.Contains(err.Error(), "sync.cadence") {
			t.Errorf("error = %v, want it to name sync.cadence — the reader has to be told "+
				"which setting is broken, not merely that something is", err)
		}
	})
}
