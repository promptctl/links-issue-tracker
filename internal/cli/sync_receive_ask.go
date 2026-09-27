package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The inline receive asks before it fetches. Every 5 minutes the first command
// to run used to pay a full DOLT_FETCH — 4.9s measured on `lit backlog` on
// 2026-09-26, against 0.4s for the commands around it — to learn the remote
// had not moved: 41 of 41 receive traces on 2026-08-25 found the store already
// up to date. One `git ls-remote <remote> refs/dolt/*` answers the same
// question with no transfer and no store open (1.2–1.3s over ssh to GitHub,
// 0.5–0.6s over https, measured 2026-09-27), so the fetch and the reconcile
// behind it run only when the advertisement differs from what the last
// settled receive recorded.
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
// runs, exactly as it did before the question existed, and the previous
// record stands. [LAW:no-silent-failure]

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

// recordReceived writes what the receive established, and only that. A fetch
// that returned without error, whatever state it reached, means a fetch
// succeeded now (fetch-success.last, which the 24-hour staleness banner
// reads). A receive that settled cleanly — that fetch, and a reconcile that
// converged if one ran — means the store now holds what the remote advertised
// before the fetch, so that advertisement becomes the record the next
// question is measured against; an unconverged divergence leaves the record
// alone, so the next receive fetches and surfaces it again. Nothing learned
// (the question failed) records nothing, so the previous record stands. The
// record has this one writer, which is what keeps a failed fetch, an
// unsettled one, or an unanswered question from ever reading as "unmoved".
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
