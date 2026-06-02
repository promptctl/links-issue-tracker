package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// recoverAttempts is the loop budget for both CLI recovery paths, because both
// supply a single fixed proposal: the deterministic mapper is a pure function of
// the dump, and an operator's --mapping is one authored file. Neither can
// self-repair within a run, so one attempt either reconciles or surfaces its
// residual. The convergence loop for the operator path is editing the file and
// re-running: each invocation surfaces what Validate found unaccounted-for,
// which is the next edit's worklist. (Recover itself takes any budget; a
// feedback-consuming mapper would pass a larger one.)
const recoverAttempts = 1

// recoverMapper selects the recovery mapper from data, not control flow: with
// no --mapping the autonomous DeterministicMapper drives recovery; with one,
// the operator's authored mapping does. Either way the downstream
// dump→apply→verify→promote pipeline is identical — only the mapper value
// differs. The operator mapping is read here (a trust boundary: external file)
// and decoded into the one ShapeMapping type; its semantic validity is left to
// Recover's Validate gate, which names any unaccounted-for column so the
// operator can complete the mapping and re-run.
func recoverMapper(mappingPath string) (store.Mapper, error) {
	if mappingPath == "" {
		return store.DeterministicMapper, nil
	}
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		return nil, fmt.Errorf("read mapping %s: %w", mappingPath, err)
	}
	var mapping store.ShapeMapping
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, fmt.Errorf("parse mapping %s: %w", mappingPath, err)
	}
	return func(store.RawDump, string) (store.ShapeMapping, error) { return mapping, nil }, nil
}

// runLifeboatRecover is the recovery path: dump the broken workspace below the
// migration gate, rebuild it at the current baseline through a mapper, verify
// conservation, and on success promote the rebuild in place. The mapper is the
// built-in deterministic one by default, or an operator-authored mapping when
// --mapping names a file — the latter is how a shape the deterministic mapper
// cannot recognize (the genuine schema-ahead deadend) is recovered, with the
// operator reasoning the correspondence the tool cannot. Both route through the
// identical pipeline. It realizes the epic's three exits as the CLI contract:
//   - Reconciled    → promote, report the backup path, exit 0.
//   - RequiresDrop  → notify once and refuse to commit (exit non-zero); the
//     unexplained drops need a human decision before any data is discarded.
//   - Unconverged   → loud failure with the residual (exit non-zero).
func runLifeboatRecover(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("lifeboat recover")
	mappingPath := fs.String("mapping", "", "Path to an operator-authored ShapeMapping JSON; default uses the built-in deterministic mapper")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit lifeboat recover [--mapping <file>]")
	}
	mapper, err := recoverMapper(*mappingPath)
	if err != nil {
		return err
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
	outcome, err := store.Recover(ctx, ws.DatabasePath, dump, mapper, recoverAttempts)
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
