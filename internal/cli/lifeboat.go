package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const lifeboatUsage = "usage: lit lifeboat <dump|recover> ..."

func validateLifeboatCommandPath(args []string) error {
	return validateNestedCommandPath(args, lifeboatUsage, "dump", "recover")
}

// runLifeboat is the data lifeboat command surface (links-recovery-j0vl): the
// recovery path that reads a workspace's data below the migration gate, so a
// workspace store.Open() refuses can still be released and rebuilt. This
// foundation registers the surface and its first verb, `dump`; later verbs in
// the epic (map/apply/verify/run) attach here.
func runLifeboat(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New(lifeboatUsage)
	}
	switch args[0] {
	case "dump":
		return runLifeboatDump(ctx, stdout, ws, args[1:])
	case "recover":
		return runLifeboatRecover(ctx, stdout, ws, args[1:])
	default:
		return errors.New(lifeboatUsage)
	}
}

// deterministicRecoverAttempts is the loop budget for the autonomous path. The
// deterministic mapper is a pure function of the dump, so re-running it cannot
// self-repair: one attempt either reconciles or surfaces its residual. The
// feedback-consuming LLM path (wired later) is the same loop with a larger
// budget.
const deterministicRecoverAttempts = 1

// runLifeboatRecover is the autonomous recovery path: dump the broken workspace
// below the migration gate, rebuild it at the current baseline through the
// deterministic mapper, verify conservation, and on success promote the rebuild
// in place. It realizes the epic's three exits as the CLI contract:
//   - Reconciled    → promote, report the backup path, exit 0.
//   - RequiresDrop  → notify once and refuse to commit (exit non-zero); the
//     unexplained drops need a human decision before any data is discarded.
//   - Unconverged   → loud failure with the residual (exit non-zero).
func runLifeboatRecover(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("lifeboat recover")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit lifeboat recover")
	}
	// [LAW:dataflow-not-control-flow] Always heal first: a prior promotion crashed
	// between its two renames leaves the canonical directory absent, which the
	// read path below would reject before the swap's own heal could run. The
	// presence of the directory is the datum that decides whether this acts; a
	// healthy workspace makes it a no-op.
	if err := store.HealWorkspace(ctx, ws.DatabasePath); err != nil {
		return err
	}
	dump, err := store.DumpRaw(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return err
	}
	outcome, err := store.Recover(ctx, ws.DatabasePath, dump, store.DeterministicMapper, deterministicRecoverAttempts)
	if err != nil {
		return err
	}
	switch o := outcome.(type) {
	case store.Reconciled:
		return promoteReconciled(ctx, stdout, ws, o)
	case store.RequiresDrop:
		// [LAW:no-silent-fallbacks] Discard the rebuild and refuse to commit: an
		// unexplained drop silently loses data, so the human is notified and
		// nothing changes on disk until they decide.
		discardErr := o.Candidate.Discard()
		return errors.Join(fmt.Errorf("recovery needs a human decision: the mapping discards %d source column(s) with no recorded justification:\n%s\nnothing was changed; supply a mapping that maps or intentionally drops these before recovering",
			len(o.Drops), formatDrops(o.Drops)), discardErr)
	case store.Unconverged:
		return fmt.Errorf("recovery did not converge after %d attempt(s); nothing was changed:\n%s", o.Attempts, o.Residual)
	default:
		return fmt.Errorf("unknown recovery outcome %T", outcome)
	}
}

func promoteReconciled(ctx context.Context, stdout io.Writer, ws workspace.Info, o store.Reconciled) (err error) {
	// [LAW:no-silent-fallbacks] Promotion has succeeded once PromoteCandidate
	// returns, but a failure to remove the candidate's scratch tree leaves residue
	// in the storage dir the operator must see; join it to the return rather than
	// discard it — the same shape snapshots restore uses for its deferred release.
	defer func() {
		if discErr := o.Candidate.Discard(); discErr != nil {
			err = errors.Join(err, fmt.Errorf("discard candidate scratch after promotion: %w", discErr))
		}
	}()
	result, err := store.PromoteCandidate(ctx, ws.DatabasePath, o.Candidate)
	if err != nil {
		return err
	}
	// The preservation clause is a function of whether a backup was actually made:
	// PromoteCandidate returns an empty Backup when the canonical directory did not
	// exist at swap time, and printing "preserved at " with a blank path would lie.
	preserved := "no previous contents to preserve"
	if result.Backup != "" {
		preserved = fmt.Sprintf("previous contents preserved at %s", result.Backup)
	}
	_, err = fmt.Fprintf(stdout, "recovered: rebuilt workspace promoted to %s (%s)\n", result.Canonical, preserved)
	return err
}

func formatDrops(drops []store.UnexplainedDrop) string {
	var b strings.Builder
	for _, d := range drops {
		fmt.Fprintf(&b, "  - %s\n", d.Column)
	}
	return strings.TrimRight(b.String(), "\n")
}

// runLifeboatDump emits the workspace's complete raw contents as a portable
// JSON artifact, read below the migration gate. Like `lit export`, it is
// JSON-only: there is no meaningful text rendering of a full database dump, and
// the artifact is consumed by tools, not read by hand.
func runLifeboatDump(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("lifeboat dump")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit lifeboat dump")
	}
	dump, err := store.DumpRaw(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return err
	}
	return writeJSON(stdout, dump)
}
