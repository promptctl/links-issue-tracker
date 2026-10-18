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

// forkPointBranch is the branch holding the fork point of
// remotes/<remote>/<branch>.
func forkPointBranch(remote, branch string) string {
	return forkPointPrefix + remote + "/" + branch
}

// recordForkPoints runs before a fetch moves remotes/<remote>/*: each
// tracking ref the active branch shares history with becomes its fork point.
// A tracking ref the branch shares no history with leaves its fork point as
// it was, and that is what makes the record durable across the jump: the
// fetch that brings a re-rooted remote leaves the ref unrelated, so neither a
// second fetch nor a crash before the reconcile can replace the head the
// reconcile needs. [LAW:no-ambient-temporal-coupling] the record is written
// before the move, never reconstructed after it.
func (s *Store) recordForkPoints(ctx context.Context, remote string) ([]storage.ForkPointRecord, error) {
	local, err := activeBranch(ctx, s.db)
	if err != nil {
		return nil, err
	}
	trackingPrefix := "remotes/" + remote + "/"
	tracked, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_remote_branches`, trackingPrefix)
	if err != nil {
		return nil, err
	}
	recorded, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, forkPointBranch(remote, ""))
	if err != nil {
		return nil, err
	}
	records := make([]storage.ForkPointRecord, 0, len(tracked))
	for _, branch := range slices.Sorted(maps.Keys(tracked)) {
		head := tracked[branch]
		record := storage.ForkPointRecord{Remote: remote, Branch: branch, Head: head, Outcome: storage.ForkPointUnchanged}
		if recorded[branch] != head {
			record.Outcome, err = s.recordForkPoint(ctx, local, forkPointBranch(remote, branch), head)
			if err != nil {
				return nil, err
			}
		}
		records = append(records, record)
	}
	return records, nil
}

// recordForkPoint sets the fork point to head when the local branch shares
// history with head, and otherwise leaves it as it was.
func (s *Store) recordForkPoint(ctx context.Context, local, forkPoint, head string) (storage.ForkPointOutcome, error) {
	base, err := mergeBase(ctx, s.db, local, head)
	if err != nil {
		return "", err
	}
	if _, shared := base.shared(); !shared {
		return storage.ForkPointKept, nil
	}
	if err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-f", forkPoint, head); err != nil {
		return "", fmt.Errorf("record fork point %s at %s: %w", forkPoint, head, err)
	}
	return storage.ForkPointRecorded, nil
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
// The remote continues it when every ticket at the base is still on the
// remote head or is named by a purge record. That is the one thing a merge
// against a base the remote does not descend from gets destructively wrong:
// it reads a base ticket the remote lacks as a ticket the remote removed, and
// drops it. A re-root keeps every ticket, and so passes; a remote replaced by
// a history that never held this checkout's tickets fails, and stays the
// unrelated-histories divergence it is today. [LAW:parse-dont-validate] the
// result's Source is the evidence, and a refusal says why in its counts.
func (s *Store) forkPointBase(ctx context.Context, remote, branch, localHead, remoteHead string) (storage.ReconcileBase, error) {
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
	unrelated.UnaccountedTickets, err = s.unaccountedTickets(ctx, commit, localHead, remoteHead)
	if err != nil || unrelated.UnaccountedTickets > 0 {
		return unrelated, err
	}
	return storage.ReconcileBase{Source: storage.ReconcileBaseForkPoint, Commit: commit, ForkPoint: forkPoint}, nil
}

// unaccountedTickets counts the tickets at base that remoteHead holds no row
// for — the same ticket: its id and its birth — and that no purge record on
// any of the three commits names. Purges from all three, because the merge
// applies the union of them to every side before it compares a row.
func (s *Store) unaccountedTickets(ctx context.Context, base, localHead, remoteHead string) (int, error) {
	atBase, err := issuesAtCommit(ctx, s.db, base)
	if err != nil {
		return 0, fmt.Errorf("read the fork point's tickets: %w", err)
	}
	atRemote, err := issuesAtCommit(ctx, s.db, remoteHead)
	if err != nil {
		return 0, fmt.Errorf("read the remote's tickets: %w", err)
	}
	var purges []model.Purge
	for _, commit := range []string{base, localHead, remoteHead} {
		held, err := purgesAtCommit(ctx, s.db, commit)
		if err != nil {
			return 0, fmt.Errorf("read the purge records at %s: %w", commit, err)
		}
		purges = append(purges, held...)
	}
	return countUnaccounted(atBase, atRemote, purges), nil
}

// countUnaccounted is unaccountedTickets' count over rows already read.
func countUnaccounted(base, remote map[string]model.Issue, purges []model.Purge) int {
	unaccounted := 0
	for id, ticket := range base {
		onRemote, held := remote[id]
		kept := held && onRemote.CreatedAt.Equal(ticket.CreatedAt)
		purged := slices.ContainsFunc(purges, func(p model.Purge) bool { return p.Names(ticket) })
		if !kept && !purged {
			unaccounted++
		}
	}
	return unaccounted
}

// ForkPointRecords is what every fetch since the store opened did to each fork
// point, oldest first.
func (s *Store) ForkPointRecords() []storage.ForkPointRecord {
	return slices.Clone(s.forkPoints)
}

// ReconcileBases is the base every reconcile plan since the store opened
// decided on, oldest first.
func (s *Store) ReconcileBases() []storage.ReconcileBase {
	return slices.Clone(s.reconcileBases)
}
