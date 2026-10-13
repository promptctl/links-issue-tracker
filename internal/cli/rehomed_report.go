package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// writeRehomed reports the tickets a write that landed a backlog whole — a
// sync, a restore — found outside an epic and moved into one, as one line after
// the line that reports the write. It prints nothing when nothing moved, so an
// ordinary sync or restore gains no line; the trace carries the count either
// way (rehomedMetadata). A move to a nearest epic is usually home already; a
// move to the unfiled epic is a ticket waiting to be filed, so the line names
// how. Ids may have arrived from a remote, so each is quoted like every
// remote-authored id.
func writeRehomed(w io.Writer, backlog string, rehomed []storage.Rehomed) error {
	if len(rehomed) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "moved %d ticket(s) %s held outside an epic: %s; `lit parent set --child <id> --parent <epic-id>` files one where it belongs\n",
		len(rehomed), backlog, strings.Join(describeMoves(rehomed, func(id string) string { return quoteRemote(id).inline() }), ", "))
	return err
}

// rehomedMetadata records the moves on a trace record: the count, zero
// included, and each move. [LAW:nothing-unseen] zero moves and an unrecorded
// write read differently.
func rehomedMetadata(metadata map[string]string, rehomed []storage.Rehomed) {
	metadata["rehomed"] = strconv.Itoa(len(rehomed))
	metadata["rehomed_moves"] = strings.Join(describeMoves(rehomed, func(id string) string { return id }), ", ")
}

// describeMoves renders each move as "<ticket> into <epic>", or "<epic> to the
// top level" for an epic that had no epic above it.
func describeMoves(rehomed []storage.Rehomed, render func(string) string) []string {
	moves := make([]string, 0, len(rehomed))
	for _, r := range rehomed {
		if r.Epic == "" {
			moves = append(moves, render(r.IssueID)+" to the top level")
			continue
		}
		moves = append(moves, render(r.IssueID)+" into "+render(r.Epic))
	}
	return moves
}
