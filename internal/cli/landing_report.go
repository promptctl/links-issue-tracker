package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// writeLanding reports what a write that landed a backlog whole — a sync, a
// restore — had to change on the way in, after the line that reports the
// write. It prints nothing when nothing changed, so an ordinary sync or restore
// gains no line; the trace carries the counts either way (landingMetadata).
func writeLanding(w io.Writer, backlog string, landing storage.Landing) error {
	if err := writeUnlooped(w, backlog, landing.Unlooped); err != nil {
		return err
	}
	if err := writeRehomed(w, backlog, landing.Rehomed); err != nil {
		return err
	}
	return writeRekeyed(w, backlog, landing.Rekeyed)
}

// writeUnlooped reports the parent edges a landing cut because each closed a
// loop: a header with the count, then one line per cut naming the loop it
// broke. The landing chose which edge goes, and the parent it took is the one
// place the child cannot go back to without closing the loop again, so the
// header names the reparent and leaves the epic to the operator. Ids may have
// arrived from a remote, so each is quoted like every remote-authored id.
func writeUnlooped(w io.Writer, backlog string, unlooped []storage.Unlooped) error {
	if len(unlooped) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "cut %d parent edge(s) %s held in a loop, which has no root; `lit parent set --child <id> --parent <epic-id>` moves a child where it belongs:\n", len(unlooped), backlog); err != nil {
		return err
	}
	for _, cut := range describeCuts(unlooped, func(id string) string { return quoteRemote(id).inline() }) {
		if _, err := fmt.Fprintf(w, "  %s\n", cut); err != nil {
			return err
		}
	}
	return nil
}

// writeRehomed reports the tickets a landing found outside an epic and moved
// into one: a header with the count, then one line per move, so a restore that
// homes hundreds still reads and greps a move at a time. A move to a nearest
// epic is usually home already; a move to the unfiled epic is a ticket waiting
// to be filed, so the line names how. Ids may have arrived from a remote, so
// each is quoted like every remote-authored id.
func writeRehomed(w io.Writer, backlog string, rehomed []storage.Rehomed) error {
	if len(rehomed) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "moved %d ticket(s) %s held outside an epic; `lit parent set --child <id> --parent <epic-id>` files one where it belongs:\n", len(rehomed), backlog); err != nil {
		return err
	}
	for _, move := range describeMoves(rehomed, func(id string) string { return quoteRemote(id).inline() }) {
		if _, err := fmt.Fprintf(w, "  %s\n", move); err != nil {
			return err
		}
	}
	return nil
}

// writeRekeyed reports the tickets a landing gave a new rank key so that no two
// share one, on one line: no listing changed its order, so there is nothing
// left for the reader to do but know it happened.
func writeRekeyed(w io.Writer, backlog string, rekeyed []storage.Rekeyed) error {
	if len(rekeyed) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "gave %d ticket(s) in %s a new rank key so that no two share one; every listing keeps its order: %s\n", len(rekeyed), backlog, strings.Join(rekeyedIDs(rekeyed, func(id string) string { return quoteRemote(id).inline() }), ", "))
	return err
}

// landingMetadata records what a landing changed on a trace record: each count,
// zero included, and each change. [LAW:nothing-unseen] zero changes and an
// unrecorded write read differently.
func landingMetadata(metadata map[string]string, landing storage.Landing) {
	identity := func(id string) string { return id }
	metadata["unlooped"] = strconv.Itoa(len(landing.Unlooped))
	metadata["unlooped_cuts"] = strings.Join(describeCuts(landing.Unlooped, identity), ", ")
	metadata["rehomed"] = strconv.Itoa(len(landing.Rehomed))
	metadata["rehomed_moves"] = strings.Join(describeMoves(landing.Rehomed, identity), ", ")
	metadata["rekeyed"] = strconv.Itoa(len(landing.Rekeyed))
	metadata["rekeyed_moves"] = strings.Join(describeRekeys(landing.Rekeyed), ", ")
}

// rekeyedIDs renders the id of each ticket a landing gave a key of its own.
func rekeyedIDs(rekeyed []storage.Rekeyed, render func(string) string) []string {
	ids := make([]string, len(rekeyed))
	for i, r := range rekeyed {
		ids[i] = render(r.IssueID)
	}
	return ids
}

// describeRekeys renders each new key as "<ticket> <from> -> <rank>", each key
// quoted: a key that arrived from a remote may be malformed.
func describeRekeys(rekeyed []storage.Rekeyed) []string {
	described := make([]string, len(rekeyed))
	for i, r := range rekeyed {
		described[i] = fmt.Sprintf("%s %q -> %q", r.IssueID, r.From, r.Rank)
	}
	return described
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

// describeCuts renders each cut as "<child> no longer under <parent> (loop
// <a> -> <b>)".
func describeCuts(unlooped []storage.Unlooped, render func(string) string) []string {
	cuts := make([]string, len(unlooped))
	for i, cut := range unlooped {
		loop := make([]string, len(cut.Cycle))
		for j, id := range cut.Cycle {
			loop[j] = render(id)
		}
		cuts[i] = fmt.Sprintf("%s no longer under %s (loop %s)", render(cut.IssueID), render(cut.Parent), strings.Join(loop, " -> "))
	}
	return cuts
}
