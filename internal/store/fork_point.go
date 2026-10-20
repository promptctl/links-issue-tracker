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
// its name: its first segment. For a name such as /x that segment is empty,
// and Dolt reads no remote off such refs, so that owner owns none.
func ownerOfHeld(name string) refOwner {
	first, _, _ := strings.Cut(name, "/")
	return refOwner{name: first}
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
// before the move, never reconstructed after it. tracked is remote's tracking
// refs as trackingRefs lists them ahead of the fetch.
func (s *Store) recordForkPoints(ctx context.Context, remote refOwner, tracked map[string]string) ([]storage.ForkPointRecord, error) {
	local, err := activeBranch(ctx, s.db)
	if err != nil {
		return nil, err
	}
	recorded, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(remote, ""))
	if err != nil {
		return nil, err
	}
	view := engineForkPoints{db: s.db, local: local}
	// The records written before a failure are returned with it: each one
	// moved a branch that stands. [LAW:nothing-unseen]
	records := make([]storage.ForkPointRecord, 0, len(tracked))
	for _, branch := range slices.Sorted(maps.Keys(tracked)) {
		record, err := recordForkPoint(ctx, view, remote, branch, tracked[branch], recorded)
		if err != nil {
			return records, err
		}
		records = append(records, record)
	}
	return records, nil
}

// forkPointStore is what recording a fork point reads and writes: whether the
// local branch shares history with a head, and the fork point branch itself.
// A store open on its SQL engine is one (engineForkPoints); a held chunk store
// with no engine, as the receive's landing has, is the other
// (chunkForkPoints).
type forkPointStore interface {
	sharesHistory(ctx context.Context, head string) (bool, error)
	setForkPoint(ctx context.Context, name, head string) error
}

// recordForkPoint is the one rule a fork point follows before its tracking
// ref moves off head: unchanged when it already names head; set to head when
// the local branch shares history with head; otherwise left as it was — kept
// when one was recorded, none when none ever was. recorded is the store's
// fork points of remote, branch to head. [LAW:single-enforcer] a fetch and a
// landing record by this one rule, whichever store they hold.
func recordForkPoint(ctx context.Context, st forkPointStore, remote refOwner, branch, head string, recorded map[string]string) (storage.ForkPointRecord, error) {
	record := storage.ForkPointRecord{Remote: remote.String(), Branch: branch, Head: head, Outcome: storage.ForkPointUnchanged}
	held, ok := recorded[branch]
	if held == head {
		return record, nil
	}
	shared, err := st.sharesHistory(ctx, head)
	if err != nil {
		return storage.ForkPointRecord{}, err
	}
	switch {
	case shared:
		name := forkPointBranch(remote, branch)
		if err := st.setForkPoint(ctx, name, head); err != nil {
			return storage.ForkPointRecord{}, fmt.Errorf("record fork point %s at %s: %w", name, head, err)
		}
		record.Outcome = storage.ForkPointRecorded
	case ok:
		record.Outcome = storage.ForkPointKept
	default:
		record.Outcome = storage.ForkPointNone
	}
	return record, nil
}

// engineForkPoints is a forkPointStore on an open store's SQL engine, whose
// local branch is local.
type engineForkPoints struct {
	db    *sql.DB
	local string
}

func (e engineForkPoints) sharesHistory(ctx context.Context, head string) (bool, error) {
	base, err := mergeBase(ctx, e.db, e.local, head)
	if err != nil {
		return false, err
	}
	_, shared := base.shared()
	return shared, nil
}

func (e engineForkPoints) setForkPoint(ctx context.Context, name, head string) error {
	return execProcedureDiscard(ctx, e.db, "DOLT_BRANCH", "-f", name, head)
}

// trackingRefs is owner's tracking refs, branch to head.
func (s *Store) trackingRefs(ctx context.Context, owner refOwner) (map[string]string, error) {
	return refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_remote_branches`, trackingRefPrefix(owner))
}

// prunedRefs is what a pruning fetch of owner removed: each tracking ref in
// before, listed ahead of the fetch, that after, listed behind it, lacks.
func prunedRefs(owner refOwner, before, after map[string]string) []storage.PrunedRef {
	var pruned []storage.PrunedRef
	for _, branch := range slices.Sorted(maps.Keys(before)) {
		if _, ok := after[branch]; !ok {
			pruned = append(pruned, storage.PrunedRef{Remote: owner.String(), Branch: branch, Head: before[branch]})
		}
	}
	return pruned
}

// orphanForkPoints is the rule that a fork point lives exactly as long as its
// tracking ref: of the recorded fork points, branch to head, those whose
// branch has no tracking ref in tracked. Every path that deletes a fork point
// decides here, the SQL one (dropForkPoints) and the landing's, which opens no
// SQL engine (LandFetchedHead). [LAW:single-enforcer]
func orphanForkPoints(tracked, recorded map[string]string) map[string]string {
	orphans := map[string]string{}
	for branch, head := range recorded {
		if _, ok := tracked[branch]; !ok {
			orphans[branch] = head
		}
	}
	return orphans
}

// dropForkPoints deletes every fork point of remote whose branch has no
// tracking ref in tracked (orphanForkPoints). Each deletion stands once made,
// so a retried run deletes only what is left.
func (s *Store) dropForkPoints(ctx context.Context, remote refOwner, tracked map[string]string) ([]storage.ForkPointRecord, error) {
	recorded, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(remote, ""))
	if err != nil {
		return nil, err
	}
	orphans := orphanForkPoints(tracked, recorded)
	var dropped []storage.ForkPointRecord
	for _, branch := range slices.Sorted(maps.Keys(orphans)) {
		if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", forkPointBranch(remote, branch)); err != nil {
			return dropped, fmt.Errorf("drop fork point %s: %w", forkPointBranch(remote, branch), err)
		}
		dropped = append(dropped, storage.ForkPointRecord{Remote: remote.String(), Branch: branch, Head: orphans[branch], Outcome: storage.ForkPointDropped})
	}
	return dropped, nil
}

// dropTrackingRefs deletes every tracking ref owner has and returns how many
// it deleted, a failure included. Each deletion stands once made, so a
// retried run deletes only what is left.
func (s *Store) dropTrackingRefs(ctx context.Context, owner refOwner) (int, error) {
	tracked, err := s.trackingRefs(ctx, owner)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, branch := range slices.Sorted(maps.Keys(tracked)) {
		path := owner.String() + "/" + branch
		if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", "-r", path); err != nil {
			return dropped, fmt.Errorf("drop tracking ref remotes/%s: %w", path, err)
		}
		dropped++
	}
	return dropped, nil
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

// SettledLastWrites is every settle's write of its settled.last record since
// the store opened, oldest first.
func (s *Store) SettledLastWrites() []storage.SettledLastWrite {
	return slices.Clone(s.settledLastWrites)
}
