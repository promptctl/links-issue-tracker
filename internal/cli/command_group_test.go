package cli

import (
	"context"
	"io"
	"testing"
)

// The state-transition surface splits across two help groups so the high-traffic
// status lifecycle stands out: the core verbs stay in Agent Operations, the rare
// retention verbs move to their own Issue Retention group. This pins that split —
// the acceptance criterion for regrouping the transition verbs — against the one
// registry both `--help` and completion read. [LAW:one-source-of-truth]
func TestTransitionVerbGrouping(t *testing.T) {
	specs := commandSpecs(context.Background(), io.Discard, io.Discard)
	group := map[string]string{}
	for _, s := range specs {
		group[s.Name] = s.GroupID
	}

	for _, name := range []string{"start", "done", "close", "open"} {
		if got := group[name]; got != "operations" {
			t.Fatalf("core lifecycle verb %q is in group %q, want operations", name, got)
		}
	}
	for _, name := range []string{"archive", "unarchive", "delete", "restore"} {
		if got := group[name]; got != "retention" {
			t.Fatalf("retention verb %q is in group %q, want retention (demoted out of operations)", name, got)
		}
	}

	// The retention group must be registered and demoted below Agent Operations,
	// or the GroupID above would render ungrouped / above the core verbs.
	opsIdx, retIdx := -1, -1
	for i, g := range commandGroups {
		switch g.ID {
		case "operations":
			opsIdx = i
		case "retention":
			retIdx = i
		}
	}
	if retIdx == -1 {
		t.Fatal("commandGroups has no retention group, but retention verbs reference it")
	}
	if opsIdx == -1 || retIdx <= opsIdx {
		t.Fatalf("retention group index = %d, want below operations index %d", retIdx, opsIdx)
	}
}
