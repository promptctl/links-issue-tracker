package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// replacementMetadata records what a committed replace removed and changed,
// each count 0 included. [LAW:nothing-unseen]
func replacementMetadata(metadata map[string]string, replacement storage.Replacement) {
	ids := make([]string, len(replacement.Removed))
	for i, ticket := range replacement.Removed {
		ids[i] = ticket.IssueID
	}
	metadata["removed"] = strconv.Itoa(len(replacement.Removed))
	metadata["removed_ids"] = strings.Join(ids, ", ")
	landingMetadata(metadata, replacement.Landing)
}

// writeReplacement reports what a committed replace removed, then what it
// changed to land the backup.
func writeReplacement(w io.Writer, replacement storage.Replacement, snapshot string) error {
	if err := writeRemoved(w, replacement.Removed, snapshot); err != nil {
		return err
	}
	return writeLanding(w, "the backup", replacement.Landing)
}

// writeRemoved names each ticket a restore removed, the pre-restore backup
// taken to hold them, and the path that edits tickets without replacing the
// workspace — the one an agent restoring an edited export wanted. It prints
// nothing when the restore removed none. A ticket may have arrived from another
// clone, so the list is fenced as remote-authored prose: a title is free text.
//
// Whether a restored file is a backup being recovered or an export an agent
// edited cannot be told from its content — the sync merge stamps a row with the
// later of two sides while taking each field from either (merge.resolve), so no
// stamp proves a hand edit — and so restore reports what it removes every time,
// and refuses nothing for it.
func writeRemoved(w io.Writer, removed []storage.Removed, snapshot string) error {
	if len(removed) == 0 {
		return nil
	}
	tickets := make([]string, len(removed))
	for i, ticket := range removed {
		tickets[i] = ticket.IssueID + "  " + ticket.Title
	}
	_, err := fmt.Fprintf(w, "removed %d ticket(s) the workspace held and the file does not; the pre-restore backup %s still holds them:\n%s\n"+
		"to edit tickets without replacing the workspace: `lit ls --format yaml > f.yaml`, edit f.yaml, `lit import --path f.yaml`\n",
		len(removed), snapshot, strings.Join(quoteRemote(strings.Join(tickets, "\n")).fenced("  "), "\n"))
	return err
}
