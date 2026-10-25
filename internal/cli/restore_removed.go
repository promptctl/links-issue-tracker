package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// removedMetadata records the tickets a committed replace removed, 0 included.
// It is written only once the replace commits, so a restore refused or failed
// before it carries no `removed` key: absent, not a zero nobody counted.
// [LAW:nothing-unseen]
func removedMetadata(metadata map[string]string, removed []storage.Removed) {
	ids := make([]string, len(removed))
	for i, ticket := range removed {
		ids[i] = ticket.IssueID
	}
	metadata["removed"] = strconv.Itoa(len(removed))
	metadata["removed_ids"] = strings.Join(ids, ", ")
}

// writeRemoved names each ticket a restore removed, the pre-restore backup
// taken to hold them, and the path that edits tickets without replacing the
// workspace — the one an agent restoring an edited export wanted. It prints
// nothing when the restore removed none. A ticket may have arrived from another
// clone, so its id and title are quoted like every remote-authored value.
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
	var lines strings.Builder
	for _, ticket := range removed {
		fmt.Fprintf(&lines, "  %s  %s\n", quoteRemote(ticket.IssueID).inline(), quoteRemote(ticket.Title).inline())
	}
	_, err := fmt.Fprintf(w, "removed %d ticket(s) the workspace held and the file does not; the pre-restore backup %s still holds them:\n%s"+
		"to edit tickets without replacing the workspace: `lit ls --format yaml > f.yaml`, edit f.yaml, `lit import --path f.yaml`\n",
		len(removed), snapshot, lines.String())
	return err
}
