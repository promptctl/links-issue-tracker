package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A fork point is the remote head this checkout last knew while its branch
// still shared history with it: git's term for the commit a branch forked
// from an upstream that was later rewritten, which `git merge-base
// --fork-point` finds in the upstream's reflog. Dolt keeps no reflog that
// outlives GC, so lit keeps the one entry it needs as a branch per tracking
// ref, lit-fork-point/<remote>/<branch>. A branch is what keeps the commit's
// chunks through every GC, and DOLT_BRANCH -f moves it in one write.
//
// It is the base a reconcile merges against once the remote is re-rooted
// above every commit this checkout holds: the two heads then share no commit,
// and the fork point is the history they last shared (forkPointBase).
const forkPointPrefix = "lit-fork-point/"

// refOwner is the remote a ref under remotes/<owner>/ or
// lit-fork-point/<owner>/ belongs to: the path's first segment, which is how
// Dolt reads a tracking ref's remote (ref.RemoteRef, and DOLT_REMOTE remove
// and DOLT_BRANCH -D -r with it). It holds no "/", so every such ref has
// exactly one owner. A remote lit syncs with is its own owner; a name Dolt
// holds with a "/" in it, which DOLT_CLONE --remote once wrote, is owned by
// its first segment. [LAW:types-are-the-program] [LAW:one-source-of-truth]
type refOwner struct {
	name string
}

func (o refOwner) String() string {
	return o.name
}

// ownerOf is a parsed remote's owner: the remote itself, since its name
// holds no "/".
func ownerOf(remote storage.RemoteName) refOwner {
	return refOwner{name: remote.String()}
}

// ownerOfHeld is the owner of the refs a remote Dolt holds writes, whatever
// its name: its first segment.
func ownerOfHeld(name string) (refOwner, error) {
	first, _, _ := strings.Cut(name, "/")
	if first == "" {
		return refOwner{}, fmt.Errorf("remote %q names no owner for its refs: its first segment is empty", name)
	}
	return refOwner{name: first}, nil
}

// forkPointBranch is the branch holding the fork point of
// remotes/<owner>/<branch>.
func forkPointBranch(owner refOwner, branch string) string {
	return forkPointPrefix + owner.String() + "/" + branch
}

// trackingRefPrefix is the prefix of every tracking ref owner has,
// remotes/<owner>/, as dolt_remote_branches names them.
func trackingRefPrefix(owner refOwner) string {
	return "remotes/" + owner.String() + "/"
}

// recordForkPoints runs before a fetch moves remotes/<remote>/*: each
// tracking ref the active branch shares history with becomes its fork point.
// A tracking ref the branch shares no history with leaves its fork point as
// it was, and that is what makes the record durable across the jump: the
// fetch that brings a re-rooted remote leaves the ref unrelated, so neither a
// second fetch nor a crash before the reconcile can replace the head the
// reconcile needs. [LAW:no-ambient-temporal-coupling] the record is written
// before the move, never reconstructed after it.
func (s *Store) recordForkPoints(ctx context.Context, remote refOwner) ([]storage.ForkPointRecord, error) {
	local, err := activeBranch(ctx, s.db)
	if err != nil {
		return nil, err
	}
	trackingPrefix := trackingRefPrefix(remote)
	tracked, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_remote_branches`, trackingPrefix)
	if err != nil {
		return nil, err
	}
	recorded, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(remote, ""))
	if err != nil {
		return nil, err
	}
	// The records written before a failure are returned with it: each one
	// moved a branch that stands. [LAW:nothing-unseen]
	records := make([]storage.ForkPointRecord, 0, len(tracked))
	for _, branch := range slices.Sorted(maps.Keys(tracked)) {
		head := tracked[branch]
		record := storage.ForkPointRecord{Remote: remote.String(), Branch: branch, Head: head, Outcome: storage.ForkPointUnchanged}
		if held, ok := recorded[branch]; held != head {
			record.Outcome, err = s.recordForkPoint(ctx, local, forkPointBranch(remote, branch), head, ok)
			if err != nil {
				return records, err
			}
		}
		records = append(records, record)
	}
	return records, nil
}

// recordForkPoint sets the fork point to head when the local branch shares
// history with head, and otherwise leaves it as it was: kept when one was
// recorded, none when none ever was.
func (s *Store) recordForkPoint(ctx context.Context, local, forkPoint, head string, held bool) (storage.ForkPointOutcome, error) {
	base, err := mergeBase(ctx, s.db, local, head)
	if err != nil {
		return "", err
	}
	if _, shared := base.shared(); !shared {
		if held {
			return storage.ForkPointKept, nil
		}
		return storage.ForkPointNone, nil
	}
	if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-f", forkPoint, head); err != nil {
		return "", fmt.Errorf("record fork point %s at %s: %w", forkPoint, head, err)
	}
	return storage.ForkPointRecorded, nil
}

// dropOrphanForkPoints deletes every fork point of remote whose tracking ref
// a fetch pruned, so no fork point keeps its commit's chunks through GC once
// nothing can read it. A fork point lives exactly as long as its tracking ref.
// [LAW:one-source-of-truth]
func (s *Store) dropOrphanForkPoints(ctx context.Context, remote refOwner) ([]storage.ForkPointRecord, error) {
	tracked, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_remote_branches`, trackingRefPrefix(remote))
	if err != nil {
		return nil, err
	}
	return s.dropForkPoints(ctx, remote, tracked)
}

// dropForkPoints deletes every fork point of remote but those of the branches
// in keep. Each deletion stands once made, so a retried run deletes only what
// is left.
func (s *Store) dropForkPoints(ctx context.Context, remote refOwner, keep map[string]string) ([]storage.ForkPointRecord, error) {
	recorded, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(remote, ""))
	if err != nil {
		return nil, err
	}
	var dropped []storage.ForkPointRecord
	for _, branch := range slices.Sorted(maps.Keys(recorded)) {
		if _, ok := keep[branch]; ok {
			continue
		}
		if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", forkPointBranch(remote, branch)); err != nil {
			return dropped, fmt.Errorf("drop fork point %s: %w", forkPointBranch(remote, branch), err)
		}
		dropped = append(dropped, storage.ForkPointRecord{Remote: remote.String(), Branch: branch, Head: recorded[branch], Outcome: storage.ForkPointDropped})
	}
	return dropped, nil
}

// dropTrackingRefs deletes every tracking ref owner has. Each deletion
// stands once made, so a retried run deletes only what is left.
func (s *Store) dropTrackingRefs(ctx context.Context, owner refOwner) error {
	tracked, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_remote_branches`, trackingRefPrefix(owner))
	if err != nil {
		return err
	}
	for _, branch := range slices.Sorted(maps.Keys(tracked)) {
		path := owner.String() + "/" + branch
		if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", "-r", path); err != nil {
			return fmt.Errorf("drop tracking ref remotes/%s: %w", path, err)
		}
	}
	return nil
}

// refHeadsUnder runs query, which selects (name, hash) rows, and maps every
// name that begins with prefix, the prefix trimmed, to its hash. The prefix
// is compared in Go rather than with LIKE, whose _ and % would widen it
// inside a remote's name.
func refHeadsUnder(ctx context.Context, db *sql.DB, query, prefix string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list refs under %s: %w", prefix, err)
	}
	defer rows.Close()
	heads := map[string]string{}
	for rows.Next() {
		var name, head string
		if err := rows.Scan(&name, &head); err != nil {
			return nil, fmt.Errorf("scan ref under %s: %w", prefix, err)
		}
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			heads[rest] = head
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list refs under %s: %w", prefix, err)
	}
	return heads, nil
}

// forkPointBase is the base of a divergence whose heads share no history: the
// merge base of the local head and the fork point of remotes/<remote>/<branch>,
// provided the remote head continues that history.
//
// The remote continues it when every ticket and every ticket event at the base
// is still on the remote head, or belongs to a ticket a purge record names.
// That is what a merge against a base the remote does not descend from gets
// destructively wrong: it reads a base ticket the remote lacks as one the
// remote removed, and a base edit the remote lacks as one the remote reverted.
// Tickets and events are the rows only a purge removes, so a re-root keeps
// every one and passes; a remote replaced by a history that never held this
// checkout's tickets or edits fails, and stays the unrelated-histories
// divergence it is today. [LAW:parse-dont-validate] the result's Source is the
// evidence, and a refusal says why in its counts.
func (s *Store) forkPointBase(ctx context.Context, remote refOwner, branch, localHead, remoteHead string) (storage.ReconcileBase, error) {
	unrelated := storage.ReconcileBase{Source: storage.ReconcileBaseNone}
	recorded, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(remote, ""))
	if err != nil {
		return storage.ReconcileBase{}, err
	}
	forkPoint, ok := recorded[branch]
	if !ok {
		return unrelated, nil
	}
	unrelated.ForkPoint = forkPoint
	fork, err := mergeBase(ctx, s.db, localHead, forkPoint)
	if err != nil {
		return storage.ReconcileBase{}, err
	}
	commit, shared := fork.shared()
	if !shared {
		return unrelated, nil
	}
	unrelated.UnaccountedTickets, unrelated.UnaccountedEvents, err = s.unaccountedRows(ctx, commit, localHead, remoteHead)
	if err != nil || unrelated.UnaccountedTickets+unrelated.UnaccountedEvents > 0 {
		return unrelated, err
	}
	return storage.ReconcileBase{Source: storage.ReconcileBaseForkPoint, Commit: commit, ForkPoint: forkPoint}, nil
}

// unaccountedRows counts the tickets at base that remoteHead holds no row for
// — the same ticket: its id and its birth — and the events at base that
// remoteHead holds no row for, leaving out each one whose ticket a purge
// record on any of the three commits names. Purges from all three, because
// the merge applies the union of them to every side before it compares a row.
func (s *Store) unaccountedRows(ctx context.Context, base, localHead, remoteHead string) (tickets, events int, err error) {
	atBase, err := issuesAtCommit(ctx, s.db, base)
	if err != nil {
		return 0, 0, fmt.Errorf("read the fork point's tickets: %w", err)
	}
	atRemote, err := issuesAtCommit(ctx, s.db, remoteHead)
	if err != nil {
		return 0, 0, fmt.Errorf("read the remote's tickets: %w", err)
	}
	eventsAtBase, err := eventTicketsAtCommit(ctx, s.db, base)
	if err != nil {
		return 0, 0, fmt.Errorf("read the fork point's ticket events: %w", err)
	}
	eventsAtRemote, err := eventTicketsAtCommit(ctx, s.db, remoteHead)
	if err != nil {
		return 0, 0, fmt.Errorf("read the remote's ticket events: %w", err)
	}
	var purges []model.Purge
	for _, commit := range []string{base, localHead, remoteHead} {
		held, err := purgesAtCommit(ctx, s.db, commit)
		if err != nil {
			return 0, 0, fmt.Errorf("read the purge records at %s: %w", commit, err)
		}
		purges = append(purges, held...)
	}
	tickets, events = countUnaccounted(atBase, atRemote, eventsAtBase, eventsAtRemote, purges)
	return tickets, events, nil
}

// countUnaccounted is unaccountedRows' count over rows already read. The
// events are keyed by event id, each naming its ticket's id.
func countUnaccounted(base, remote map[string]model.Issue, baseEvents, remoteEvents map[string]string, purges []model.Purge) (tickets, events int) {
	purged := func(ticket model.Issue) bool {
		return slices.ContainsFunc(purges, func(p model.Purge) bool { return p.Names(ticket) })
	}
	for id, ticket := range base {
		onRemote, held := remote[id]
		if !(held && onRemote.CreatedAt.Equal(ticket.CreatedAt)) && !purged(ticket) {
			tickets++
		}
	}
	// An event whose ticket the base does not hold records no edit the merge
	// could revert: the merge carries events with their ticket.
	for id, ticketID := range baseEvents {
		ticket, onBase := base[ticketID]
		if _, held := remoteEvents[id]; !held && onBase && !purged(ticket) {
			events++
		}
	}
	return tickets, events
}

// ForkPointRecords is what every fetch, every collection below a cut and
// every remote removal since the store opened did to each fork point, oldest
// first.
func (s *Store) ForkPointRecords() []storage.ForkPointRecord {
	return slices.Clone(s.forkPoints)
}

// ReconcileBases is the base every reconcile plan since the store opened
// decided on, oldest first.
func (s *Store) ReconcileBases() []storage.ReconcileBase {
	return slices.Clone(s.reconcileBases)
}

// StrandedWritesCommits is every commit freshnessOverEveryWrite landed since
// the store opened, oldest first.
func (s *Store) StrandedWritesCommits() []storage.StrandedWritesCommit {
	return slices.Clone(s.strandedWritesCommits)
}
