package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// purgeLeaf is `lit purge <id>`: the one command that removes a ticket for
// good. The confirmation gate follows `start --take`'s shape — the flag
// confirms wherever it is passed, a terminal is asked, and a caller with no
// terminal and no flag is refused with a remediation that names the flag and
// tells an agent the purge is the user's call.
func purgeLeaf() appLeaf {
	fs := newCobraFlagSet("purge").Detail(helpText("purge"))
	yes := fs.Bool("yes", false, "Confirm the purge (required for non-interactive callers; without it an interactive terminal is prompted instead)")
	resolveActor := registerActor(fs)
	const usage = "usage: lit purge <id> [--yes]"
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usage}
		}
		// Read first, so a missing id is reported before anyone is asked to
		// confirm.
		issue, err := ap.Store.GetIssue(ctx, positional[0])
		if err != nil {
			return err
		}
		if err := confirmPurge(stdout, issue.ID, *yes); err != nil {
			return err
		}
		result, err := ap.Store.Purge(ctx, storage.PurgeInput{IssueID: issue.ID, Actor: resolveActor()})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "purged %s (comments=%d labels=%d edges=%d events=%d)\n",
			result.Purge.IssueID, result.Comments, result.Labels, result.Relations, result.Events)
		return err
	}}
}

// confirmPurge is the purge's crossing: --yes confirms it, a terminal is
// asked, and anything else is refused. Neither the prompt nor any other line
// the command prints shows the ticket's text: a ticket is often purged because
// of what it says, and terminal output lands in scrollback, captures and logs.
func confirmPurge(stdout io.Writer, id string, yes bool) error {
	if yes {
		return nil
	}
	if !isTerminal(stdout) {
		return purgeUnconfirmedError{Message: fmt.Sprintf("purging %s cannot be undone; pass --yes to confirm it", id)}
	}
	confirmed, err := askYesNo(stdout, fmt.Sprintf("%s: a purge removes it, its comments, labels, history and edges for good, here and in every store it syncs to\npurge it?", id))
	if err != nil {
		return err
	}
	if !confirmed {
		return purgeUnconfirmedError{Message: "purge declined"}
	}
	return nil
}

// purgeUnconfirmedError is the purge gate's refusal: no --yes off a terminal,
// or a "no" at the prompt. The act that clears both is the same flag, so one
// type carries both, and its sinks name --yes rather than the default advice
// to retry. [LAW:one-type-per-behavior]
type purgeUnconfirmedError struct {
	Message string
}

func (e purgeUnconfirmedError) Error() string { return e.Message }
