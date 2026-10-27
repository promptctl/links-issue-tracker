package cli

import (
	"context"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// unquarantineLeaf clears the quarantine a failed migration left, the
// non-lossy way out of the refusal every opening command meets. It is a
// workspace leaf, not an app leaf: the app's open is what the quarantine
// refuses. [LAW:locality-or-seam]
func unquarantineLeaf() wsLeaf {
	fs := newCobraFlagSet("unquarantine")
	return wsLeaf{fs: fs, positionals: 0, usage: "usage: lit unquarantine", work: func(ctx context.Context, inv Invocation, ws workspace.Info, _ []string) error {
		done, err := store.Unquarantine(ctx, inv.Env, inv.Stderr, ws.DatabasePath, ws.WorkspaceID)
		if err != nil {
			return err
		}
		facts, counts := done.EventRecord()
		telemetry.Record(ctx, facts, counts)
		_, err = fmt.Fprintln(inv.Stdout, done)
		return err
	}}
}
