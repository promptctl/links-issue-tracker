package cli

import (
	"context"
	"fmt"
	"io"

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
	return wsLeaf{fs: fs, positionals: 0, usage: "usage: lit unquarantine", work: func(ctx context.Context, stdout io.Writer, ws workspace.Info, _ []string) error {
		done, err := store.Unquarantine(ctx, ws.DatabasePath, ws.WorkspaceID)
		if err != nil {
			return err
		}
		facts, counts := done.EventRecord()
		telemetry.Record(ctx, facts, counts)
		_, err = fmt.Fprintf(stdout, "cleared the quarantine of migration v%d %q\n%s\n", done.Cleared.Version, done.Cleared.Name, unquarantineNext(done.Next))
		return err
	}}
}

// unquarantineNext is what the next open meets: the cleared migration run
// again, or the quarantine still in force refusing it first.
func unquarantineNext(next *store.QuarantineBlockError) string {
	if next == nil {
		return "the next command that opens the store runs it again"
	}
	return fmt.Sprintf("migration v%d %q is still quarantined, so the next command that opens the store refuses on it; `lit unquarantine` clears it", next.Version, next.Name)
}
