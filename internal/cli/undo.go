package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// undoFamily reverts writes after the fact, from the store's own history:
// `list` shows the newest writes and what each moved, `revert` puts back what
// the named writes moved, as one bulk edit.
var undoFamily = commandFamily[appSubcommand]{
	usage: "usage: lit undo <list|revert> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "list", payload: appSubcommand{access: app.AccessRead, declare: undoListLeaf}},
		{name: "revert", payload: appSubcommand{access: app.AccessWrite, declare: undoRevertLeaf}},
	},
}

func undoListLeaf() appLeaf {
	fs := newCobraFlagSet("undo list")
	last := fs.Int("last", 20, "How many of the newest writes to list")
	const usage = "usage: lit undo list [--last <n>]"
	return appLeaf{fs: fs, positionals: 0, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if *last < 1 {
			return model.ValidationError{Message: fmt.Sprintf("--last must be at least 1, not %d", *last)}
		}
		reverter, err := storage.Undo.Of(ap.Store)
		if err != nil {
			return err
		}
		writes, err := reverter.ListWrites(ctx, *last)
		if err != nil {
			return err
		}
		revertable := 0
		for _, w := range writes {
			if len(w.Unrevertable) == 0 {
				revertable++
			}
			if err := printWrite(inv.Stdout, w); err != nil {
				return err
			}
		}
		// [LAW:nothing-unseen] zero writes listed and none revertable are
		// facts, distinct from the command not having run.
		telemetry.Set(ctx, "undo.listed", len(writes))
		telemetry.Set(ctx, "undo.revertable", revertable)
		return nil
	}}
}

// printWrite shows one write: its id, when, and its message, then each slot
// it moved and what of it no document can put back.
func printWrite(stdout io.Writer, w storage.WriteSummary) error {
	if _, err := fmt.Fprintf(stdout, "%s  %s  %s\n", w.ID, w.At.UTC().Format(time.RFC3339), w.Message); err != nil {
		return err
	}
	for _, move := range w.Moves {
		if _, err := fmt.Fprintf(stdout, "  %s\n", move); err != nil {
			return err
		}
	}
	for _, u := range w.Unrevertable {
		if _, err := fmt.Fprintf(stdout, "  cannot undo: %s\n", unrevertableLine(u)); err != nil {
			return err
		}
	}
	return nil
}

func unrevertableLine(u storage.Unrevertable) string {
	if u.Reverse == "" {
		return u.What
	}
	return fmt.Sprintf("%s (%s)", u.What, u.Reverse)
}

func undoRevertLeaf() appLeaf {
	fs := newCobraFlagSet("undo revert")
	writesFlag := fs.StringArray("writes", "Write ids from `lit undo list`, comma-separated or repeated")
	resolveActor := registerActor(fs)
	const usage = "usage: lit undo revert --writes <id,id,...>"
	return appLeaf{fs: fs, positionals: 0, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		ids, err := parseWriteSet(*writesFlag)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return UsageError{Message: usage}
		}
		reverter, err := storage.Undo.Of(ap.Store)
		if err != nil {
			return err
		}
		telemetry.Set(ctx, "undo.requested", ids)
		result, err := reverter.RevertWrites(ctx, ids, resolveActor(inv.Env))
		recordRevert(ctx, result, err)
		if err != nil {
			return err
		}
		return printRevert(inv.Stdout, result)
	}}
}

// recordRevert puts what an undo decided on the command's event: what it put
// back and landed, or how many unrevertable changes or conflicts refused it.
// [LAW:nothing-unseen]
func recordRevert(ctx context.Context, result storage.RevertResult, err error) {
	// A refusal of the other kind, or none, leaves its value zero: a count
	// of none, which is the fact.
	var refused storage.RevertRefusedError
	var conflicted storage.ConflictError
	errors.As(err, &refused)
	errors.As(err, &conflicted)
	telemetry.Set(ctx, "undo.unrevertable", len(refused.Unrevertable))
	telemetry.Set(ctx, "undo.conflicts", len(conflicted.Conflicts))
	telemetry.Set(ctx, "undo.reverted", result.Writes)
	telemetry.Set(ctx, "undo.slots", len(result.Moves))
	telemetry.Set(ctx, "undo.landed", result.Landed)
	recordBulkApplied(ctx, "undo", result.Applied)
}

func printRevert(stdout io.Writer, result storage.RevertResult) error {
	if err := printIDs(stdout, fmt.Sprintf("reverted %d writes", len(result.Writes)), result.Writes); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "put back %d\n", len(result.Moves)); err != nil {
		return err
	}
	for _, move := range result.Moves {
		if _, err := fmt.Fprintf(stdout, "  %s\n", move); err != nil {
			return err
		}
	}
	if err := printBulkApplied(stdout, result.Applied); err != nil {
		return err
	}
	return printIDs(stdout, fmt.Sprintf("landed as %d writes (revert them to undo this undo)", len(result.Landed)), result.Landed)
}
