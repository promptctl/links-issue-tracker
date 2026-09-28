package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The inline receive asks before it fetches, rather than paying a full
// DOLT_FETCH to learn the remote has not moved. One `git ls-remote <remote>
// refs/dolt/*` answers the same question with no transfer and no store open
// (1.2–1.3s over ssh to GitHub, 0.5–0.6s over https, measured 2026-09-27), so
// the fetch and the reconcile behind it run only when the advertisement
// differs from what the last settled receive recorded.
//
// The record says what was last RECEIVED, never what was last seen: it is
// written only after a receive settled cleanly — the DOLT_FETCH returned
// without error and any reconcile it led to converged — and it holds the
// advertisement observed before that fetch, so it can lag the store but never
// lead it. A record written any earlier would let one failed fetch convince
// every later command the remote had not moved; a record written over an
// unconverged divergence would stop the receive re-fetching and re-surfacing
// it every interval. The record lives beside the Dolt directory and is
// forgotten by every rotation of it (store.LockWorkspaceExclusive), so a
// restored snapshot never inherits a record that says more than it holds. A
// question that cannot be answered — the remote unreachable, the record
// unreadable — is not "nothing changed": it is traced and the full fetch
// runs, and the previous record stands. [LAW:no-silent-failure]
//
// A push writes the record too, because a push moves the remote as surely as
// a peer does: otherwise every push this checkout makes (the mirror after each
// write, `lit sync push`) would make the next receive see a moved remote and
// fetch, only to find the store already holds everything. A push that landed
// without being superseded asks the remote what it now advertises and records
// it only when the store's own git mirror holds every advertised commit
// (provePushedAdvertisement). The remote's head is then the push's own
// write, never a peer's later push that the mirror has never seen, so the
// record still never leads the store.

// remoteAdvertisement is what one round trip to the sync remote learned: which
// remote answered, at what URL, and the refs/dolt/* listing it advertised. The
// name and URL travel with the listing so a re-pointed remote can never read as
// unmoved on the strength of an old listing. The zero value is "nothing
// learned": it matches no record and records nothing, so a receive that
// fetched without getting to ask leaves the record it found standing.
// [LAW:types-are-the-program]
type remoteAdvertisement struct {
	remote string
	url    string
	refs   string
}

// record is the marker payload for this advertisement; nil for the zero value.
func (a remoteAdvertisement) record() []byte {
	if a == (remoteAdvertisement{}) {
		return nil
	}
	return []byte(a.remote + " " + a.url + "\n" + a.refs + "\n")
}

// unmoved reports whether this advertisement is byte-for-byte what the last
// settled receive recorded. Nothing learned never matches anything, and an
// absent or empty record matches nothing, so the only way to skip the fetch
// is a real answer equal to a real record. [LAW:parse-dont-validate]
func (a remoteAdvertisement) unmoved(recorded []byte) bool {
	want := a.record()
	return want != nil && bytes.Equal(want, recorded)
}

// readReceivedRefs returns the record, nil when no receive has recorded one.
// Any other read failure is reported and reads as nil: the remote is then
// fetched, the safe side of "could not tell". [LAW:no-silent-failure]
func readReceivedRefs(ws workspace.Info) []byte {
	payload, err := store.ReadReceivedRefs(ws.DatabasePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: received-refs marker unreadable: %v\n", err)
		return nil
	}
	return payload
}

// askRemote is the one round trip. It resolves the sync remote the way the
// store-backed path does — the upstream remote, else the single remote — and
// lists the refs/dolt/* that remote advertises, with the store still closed.
// A remote the store-free rule cannot pick (several remotes, no upstream)
// is the zero advertisement, not an error: it is the syncTargetNoRemote skip
// the full path already owns, so the caller falls through to it.
// [LAW:one-source-of-truth] resolveSyncRemote is the one selection rule, so
// the remote asked here is the remote the fetch would have used.
func askRemote(ctx context.Context, ws workspace.Info, gitRemotes []workspace.GitRemote) (remoteAdvertisement, error) {
	remoteName, err := resolveSyncRemote("", workspace.UpstreamRemote(ctx, ws.RootDir), gitRemotes)
	if err != nil {
		return remoteAdvertisement{}, err
	}
	if remoteName == "" {
		return remoteAdvertisement{}, nil
	}
	return advertise(ctx, ws, remoteName, gitRemotes)
}

// advertise is the round trip itself, against a named remote: the listing
// comes from `git ls-remote`, the URL from the git config it was read from.
// [LAW:one-source-of-truth] the receive's question and the push's proof both
// build their advertisement here, so the two can only ever agree byte for byte.
func advertise(ctx context.Context, ws workspace.Info, remoteName string, gitRemotes []workspace.GitRemote) (remoteAdvertisement, error) {
	refs, err := workspace.RemoteDoltRefs(ctx, ws.RootDir, remoteName)
	if err != nil {
		return remoteAdvertisement{}, fmt.Errorf("list refs/dolt/* on remote %q: %w", remoteName, err)
	}
	return remoteAdvertisement{
		remote: remoteName,
		url:    mapGitRemotesByName(gitRemotes)[remoteName],
		refs:   refs,
	}, nil
}

// commits is the commit id of every ref in the listing, in listing order.
// `git ls-remote` prints one "<id>\t<ref>" line per ref.
func (a remoteAdvertisement) commits() []string {
	var ids []string
	for _, line := range strings.Split(a.refs, "\n") {
		if id, _, ok := strings.Cut(line, "\t"); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// pushProofDeadline bounds a push's proof: one `git ls-remote` (1.2-1.7s
// measured over ssh, 2026-09-27) and one local `git cat-file`. The push has
// already landed, so a proof cut short costs only the next receive one fetch.
const pushProofDeadline = 10 * time.Second

// provePushedAdvertisement asks the remote, right after a push from this
// session's store landed without being superseded, what it now advertises,
// and returns that advertisement only when the store's own git mirror holds
// every commit in it. Otherwise it returns the zero advertisement, which
// records nothing, and why, which the push's trace carries.
//
// Why holding the commit proves the advertisement is this push's: Dolt moves
// refs/dolt/data only by compare-and-swap, each write a new commit on top of
// the one it replaced, and a push that landed made the last of those writes
// from this mirror. Until a peer writes again, that commit is the remote's
// head, and it is in the mirror. A peer's later write is a commit this mirror
// has never fetched: nothing fetches into it for the rest of the push's
// session, since the mirror's clone is private to its cycle and a foreground
// push holds the store. A superseded push is left out because it did fetch,
// so its mirror can hold a peer's head that the store never took in.
//
// Only the remote the automatic receive asks is worth proving: the record
// holds one remote's advertisement, and a push to another remote would
// replace a record the receive can use with one it never matches.
// [LAW:one-source-of-truth] resolveSyncRemote picks that remote, as it does
// for the receive. [LAW:parse-dont-validate] the non-zero advertisement is the
// proof; nothing downstream re-asks.
func provePushedAdvertisement(ctx context.Context, syncer storage.Syncer, ws workspace.Info, remoteName string, gitRemotes []workspace.GitRemote) (proven remoteAdvertisement, unproven string) {
	receiveRemote, err := resolveSyncRemote("", workspace.UpstreamRemote(ctx, ws.RootDir), gitRemotes)
	if err != nil {
		return remoteAdvertisement{}, "resolve the remote the automatic receive asks: " + err.Error()
	}
	if receiveRemote != remoteName {
		return remoteAdvertisement{}, fmt.Sprintf("pushed remote %q is not the remote the automatic receive asks (%q)", remoteName, receiveRemote)
	}
	observed, err := advertise(ctx, ws, remoteName, gitRemotes)
	if err != nil {
		return remoteAdvertisement{}, err.Error()
	}
	held, err := syncer.SyncRemoteMirrorHolds(ctx, remoteName, observed.commits())
	if err != nil {
		return remoteAdvertisement{}, err.Error()
	}
	if !held {
		return remoteAdvertisement{}, "the store's git mirror does not hold every commit the remote advertises"
	}
	return observed, ""
}

// recordPushedAdvertisement writes the record a foreground push proved, on
// the store the push ran against: that session is still open, so its
// workspace hold keeps a rotation from landing before the write. The mirror's
// proof is written by store.RecordPushedHead instead, inside its own hold on
// the live store. The zero advertisement leaves the record standing.
func recordPushedAdvertisement(ws workspace.Info, proven remoteAdvertisement) {
	payload := proven.record()
	if payload == nil {
		return
	}
	if err := store.WriteReceivedRefs(ws.DatabasePath, payload); err != nil {
		fmt.Fprintf(os.Stderr, "lit: received-refs marker not written: %v\n", err)
	}
}

// recordReceived writes what the receive established, and only that. A fetch
// that returned without error, whatever state it reached, means a fetch
// succeeded now (fetch-success.last, which the 24-hour staleness banner
// reads). A receive that settled cleanly — that fetch, and a reconcile that
// converged if one ran — means the store now holds what the remote advertised
// before the fetch, so that advertisement becomes the record the next
// question is measured against; an unconverged divergence leaves the record
// alone, so the next receive fetches and surfaces it again. Nothing learned
// (the question failed) records nothing, so the previous record stands. This
// is the receive's only write of the record. The only other writer is a push
// whose advertisement was proven, so a failed fetch, an unsettled one, or an
// unanswered question never reads as "unmoved".
// [LAW:single-enforcer] [LAW:dataflow-not-control-flow] the outcome's values
// decide; the caller runs this after every fetch path.
func recordReceived(ws workspace.Info, outcome syncReceiveOutcome, observed remoteAdvertisement) {
	if outcome.fetched() {
		if err := markFetchSuccess(ws); err != nil {
			fmt.Fprintf(os.Stderr, "lit: fetch-success marker not written: %v\n", err)
		}
	}
	payload := observed.record()
	if outcome.settledCleanly() && payload != nil {
		if err := store.WriteReceivedRefs(ws.DatabasePath, payload); err != nil {
			fmt.Fprintf(os.Stderr, "lit: received-refs marker not written: %v\n", err)
		}
	}
}

// confirmRemoteUnmoved records what an unmoved answer proved: local knowledge
// of the remote is as current as a fetch would have made it, so the
// fetch-success marker moves, and the receive trace says a receive ran and
// what it decided — a skipped fetch is a decision, not silence.
// [LAW:no-silent-failure]
func confirmRemoteUnmoved(ws workspace.Info, observed remoteAdvertisement) {
	if err := markFetchSuccess(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: fetch-success marker not written: %v\n", err)
	}
	if err := recordReceiveTrace(ws, receiveDecisionRemoteUnmoved, "ok",
		"automatic receive found the remote unmoved since the last receive; nothing fetched",
		map[string]string{"remote": observed.remote}); err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive trace not recorded: %v\n", err)
	}
}
