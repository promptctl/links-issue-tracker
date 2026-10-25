package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// droppedBy is every ticket the workspace holds that the export a restore
// would land does not: what the restore destroys. Whether the file is a backup
// being recovered or an export an agent edited cannot be told from its content
// — the sync merge stamps a row with the later of two sides while taking each
// field from either (merge.resolve), so no stamp proves a hand edit — and so
// restore reports what it destroys every time, and refuses nothing for it.
func droppedBy(local, target model.Export) []model.Issue {
	held := make(map[string]bool, len(target.Issues))
	for _, issue := range target.Issues {
		held[issue.ID] = true
	}
	var dropped []model.Issue
	for _, issue := range local.Issues {
		if !held[issue.ID] {
			dropped = append(dropped, issue)
		}
	}
	return dropped
}

// absentMetadata records the tickets the workspace holds and the file does
// not. The keys name what is counted, not what became of it: on a refused
// restore those tickets survive, on a restore that replaced the store they are
// gone, and the record's decision says which.
func absentMetadata(metadata map[string]string, dropped []model.Issue) {
	ids := make([]string, len(dropped))
	for i, issue := range dropped {
		ids[i] = issue.ID
	}
	metadata["absent_from_file"] = strconv.Itoa(len(dropped))
	metadata["absent_from_file_ids"] = strings.Join(ids, ", ")
}

// writeDropped names each ticket a restore destroyed, the pre-restore backup
// that still holds it, and the path that edits tickets without replacing the
// workspace — the one an agent restoring an edited export wanted. It prints
// nothing when the restore destroyed none. A ticket may have arrived from
// another clone, so its id and title are quoted like every remote-authored
// value.
func writeDropped(w io.Writer, dropped []model.Issue, snapshot string) error {
	if len(dropped) == 0 {
		return nil
	}
	var lines strings.Builder
	for _, issue := range dropped {
		fmt.Fprintf(&lines, "  %s  %s\n", quoteRemote(issue.ID).inline(), quoteRemote(issue.Title).inline())
	}
	_, err := fmt.Fprintf(w, "removed %d ticket(s) the workspace held and the file does not; the pre-restore backup %s still holds them:\n%s"+
		"to edit tickets without replacing the workspace: `lit ls --format yaml > f.yaml`, edit f.yaml, `lit import --path f.yaml`\n",
		len(dropped), snapshot, lines.String())
	return err
}
