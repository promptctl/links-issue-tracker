package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// unfetchedStalenessThreshold is the age past which local knowledge of the
// remote — "as of last fetch" everywhere doctor prints that phrase — is stale
// enough to warn about on ordinary read commands, not only when asked via
// `lit doctor`. [LAW:one-source-of-truth] the one constant every unfetched-
// staleness check compares against, playing the same role
// version.StaleBuildThreshold plays for build age and orphanedThreshold plays
// for in-progress issue staleness.
const unfetchedStalenessThreshold = 24 * time.Hour

// fetchSuccessMarkerPath is the file whose modification time is the last
// time local knowledge of one remote was actually CONFIRMED current: a fetch
// of that remote succeeded, or it advertised the same refs the last
// successful receive already brought in (sync_receive_ask.go) — distinct from
// receiveMarkerPath, which marks an attempt (so its debounce still applies
// even to a remote that is failing every fetch). A workspace whose fetches
// keep failing must still read as stale; conflating "attempted" with
// "succeeded" would hide exactly that. One file per remote, because a fetch
// is a fact about the remote it fetched: a fetch of another remote says
// nothing about the one the banner reports on. [LAW:one-source-of-truth]
func fetchSuccessMarkerPath(ws workspace.Info, remote storage.RemoteName) string {
	return perRemotePath(ws, "fetched", remote)
}

// markFetchSuccess records "local knowledge of remote is current as of now".
// Callers are every real DOLT_FETCH call site that returned no error — the
// explicit `lit sync fetch`/`lit sync pull`, the reconcile command's
// pre-reconcile fetch, the automatic receive — and the automatic receive's
// unmoved answer, which proves the same thing without the fetch. Each marks
// the remote it fetched, whichever remote the banner reports on.
// [LAW:single-enforcer] one marker per remote, every call site writes it the
// same way. [LAW:dataflow-not-control-flow]
func markFetchSuccess(ws workspace.Info, remote storage.RemoteName) error {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for fetch-success marker: %w", err)
	}
	if err := os.WriteFile(fetchSuccessMarkerPath(ws, remote), nil, 0o644); err != nil {
		return fmt.Errorf("write fetch-success marker: %w", err)
	}
	return nil
}

// lastFetchSuccessAge reports how long ago a fetch of remote last succeeded.
// ok is false when no fetch of it has ever succeeded on this workspace —
// mirroring shouldReceiveNow's reading that a missing marker means "never
// happened", not "infinitely stale": a workspace that simply has not had
// automatic receive run yet is not flagged before it gets the chance to.
// [LAW:no-defensive-null-guards] absence is a real, distinct state, not a
// fabricated worst-case age. A stat error other than "does not exist" —
// permission denied, a corrupt storage dir — is a real operational failure,
// not evidence of "never fetched"; it is surfaced to stderr rather than
// silently folded into the same ok=false, so a broken storage dir is
// diagnosable instead of just reading as a quiet workspace.
// [LAW:no-silent-failure]
func lastFetchSuccessAge(ws workspace.Info, remote storage.RemoteName, now time.Time) (age time.Duration, ok bool) {
	info, err := os.Stat(fetchSuccessMarkerPath(ws, remote))
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "lit: fetch-success marker unreadable: %v\n", err)
		}
		return 0, false
	}
	return now.Sub(info.ModTime()), true
}

// remoteFetch is the last successful fetch of one remote: which remote, and
// how long ago.
type remoteFetch struct {
	remote storage.RemoteName
	age    time.Duration
}

// lastTargetFetch is the last successful fetch of the remote lit syncs with
// (workspaceSyncRemote), the one both staleness banners warn about. nil when
// that remote was never fetched here, or when there is no such remote to
// read: no remote configured, or remotes that cannot be read or resolved. The
// banner is best-effort, so an unresolved remote prints no fetch line, as it
// prints no freshness line; `lit doctor` names why.
// [LAW:types-are-the-program] a known fetch always names its remote.
func lastTargetFetch(ctx context.Context, ws workspace.Info, now time.Time) *remoteFetch {
	gitRemotes, err := workspace.GitRemotes(ctx, inv.Env, ws.RootDir)
	if err != nil {
		return nil
	}
	remote, ok, err := workspaceSyncRemote(ctx, ws, "", gitRemotes)
	if err != nil || !ok {
		return nil
	}
	age, ok := lastFetchSuccessAge(ws, remote, now)
	if !ok {
		return nil
	}
	return &remoteFetch{remote: remote, age: age}
}

// syncStalenessLines renders zero or more prominent warning lines from a sync
// freshness report and last-fetch age — silent drift (unpushed local changes,
// a remote nobody has checked in days) surfaced on the ordinary commands an
// agent actually runs instead of only on `lit doctor`, which nobody runs
// unasked. Pure over its inputs so the two conditions are
// unit-testable without a live store, mirroring printSyncFreshness's split from
// its own resolve step.
// [LAW:dataflow-not-control-flow]
//
// Scope, deliberately: this checks State() == SyncAhead, not "Ahead > 0" — a
// persistent SyncDiverged already gets the heavier `<agent-instructions>`
// SyncFailureError block from the reconcile machinery, and printing a second,
// lighter banner for the same root fact here would compete with that surface
// rather than add information. It also does not special-case
// SyncNeverSynced-with-unpushed-local-data — that requires knowing whether the
// local store holds anything worth protecting, which no cheap signal here
// answers; a NeverSynced workspace is left to the second condition below (a
// never-fetched-in-threshold remote still warns) rather than a speculative
// third case.
func syncStalenessLines(report doctorSyncReport, fetch *remoteFetch) []string {
	var lines []string
	if f := report.Freshness; report.Kind == doctorSyncResolved && f.State() == storage.SyncAhead {
		lines = append(lines, fmt.Sprintf(
			"sync: %d local change(s) not pushed to %s/%s, as of last fetch — run 'lit sync push'",
			f.Ahead, f.Remote, f.Branch,
		))
	}
	// The fetch age is its own read of the sync remote, so an unresolved
	// report still warns about it.
	return append(lines, fetchStalenessLines(fetch)...)
}

// fetchStalenessLines renders the stale-fetch warning, naming the remote the
// fetch is of: a fetch is per remote, not per branch. nil is no known fetch.
func fetchStalenessLines(fetch *remoteFetch) []string {
	if fetch == nil || fetch.age < unfetchedStalenessThreshold {
		return nil
	}
	return []string{fmt.Sprintf(
		"sync: last successful fetch from %s was %s ago (%s) — run 'lit sync fetch'",
		fetch.remote, humanizeCoarseDuration(fetch.age), stalenessThresholdClause(unfetchedStalenessThreshold),
	)}
}

// syncPushFailureLines renders the loud signal for a workspace whose last push
// attempt did not land.
// The ahead-count banner above cannot serve a mutating command: its own commit
// is pushed only by a mirror that runs after the process exits, so a mutating
// command is ALWAYS ahead at the moment it would print, and a banner that fires
// on every healthy mutation trains its readers to ignore it. "The last attempt
// failed" is the discriminator that separates degraded from healthy, and it
// needs no engine — exactly what a post-close, post-mutation print site can
// afford. Pure over the already-read marker state. [LAW:dataflow-not-control-flow]
func syncPushFailureLines(rec pushOutcomeRecord, age time.Duration, known bool, running string) []string {
	if !known || !rec.failed() {
		return nil
	}
	to := ""
	if rec.Remote != "" && rec.Branch != "" {
		to = " to " + rec.Remote + "/" + rec.Branch
	}
	return []string{fmt.Sprintf(
		"sync: automatic push%s is FAILING — last attempt %s ago%s: %s — changes stay on this machine until a push succeeds; run 'lit sync push'",
		to, humanizeCoarseDuration(age), pushOutcomeProvenance(rec, running), oneLineReason(rec.Reason),
	)}
}

// oneLineReason compresses a recorded failure into banner size: first line
// only, capped. The banner is the signal; the full text lives in the sync
// trace and mirror.log, which `lit doctor` points at. [LAW:one-source-of-truth]
// the banner cites the record rather than trying to be it.
func oneLineReason(reason string) string {
	const maxLen = 160
	line := reason
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "(no reason recorded)"
	}
	if runes := []rune(line); len(runes) > maxLen {
		line = string(runes[:maxLen]) + "…"
	}
	return line
}

// printStalenessWarning resolves build age, sync freshness and last-fetch age
// (the effects), then prints the resulting lines, one each. A single call adds
// the whole drift banner to a read command — deliberately per-call-site wiring
// rather than a new central hook: each read command that wants this banner adds
// the call itself.
//
// One banner, not one per kind of drift. Build drift and sync drift are the
// same fact from the reader's side — "something about this answer is older than
// you think" — and giving each its own print call at each site would leave two
// banners to keep in the same position and the same voice, with the next read
// command free to wire up one and forget the other. [LAW:single-enforcer]
//
// Best-effort throughout: an unresolved or no-remote workspace prints no
// freshness line rather than aborting the caller, because this banner is supplementary, not
// itself a diagnostic. [LAW:no-silent-failure] [LAW:effects-at-boundaries]
func printStalenessWarning(ctx context.Context, w io.Writer, ws workspace.Info, st storage.Store, now time.Time) error {
	report := resolveDoctorSyncFreshness(ctx, ws, st)
	fetch := lastTargetFetch(ctx, ws, now)
	// Build drift leads the whole banner. It is the deepest of the three: a
	// binary that predates master can be the reason the sync lines below read
	// the way they do, so a reader who takes it in first interprets everything
	// after it correctly. It is also the rarest, which is what makes the top
	// slot affordable.
	lines := resolveBuildStalenessLines(now)
	// The push-failure line leads the sync pair: it names the CAUSE (pushes are
	// failing), which the ahead-count line below only shows the accumulating
	// effect of.
	rec, pushAge, pushKnown := lastPushOutcome(ws, now)
	lines = append(lines, syncPushFailureLines(rec, pushAge, pushKnown, runningBinaryVersion())...)
	lines = append(lines, syncStalenessLines(report, fetch)...)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// printMutationSyncStalenessWarning is the staleness banner for mutating
// commands. It runs at the one dispatch seam every mutating command already
// flows through, after the command's engine has closed, so it reads only
// storage-dir markers (push outcome, the sync remote's fetch success) and
// git's remote config: the store-backed ahead
// count is both unavailable there and, per syncPushFailureLines' rationale, the
// wrong predicate for a mutating session anyway. A session that only chains
// mutations against a failing remote hears about it on its very next command.
//
// Write failures are reported to stderr, never returned: the mutation this
// banner trails is already durable, and an exit code that flips nonzero over
// a banner write (an agent piping stdout through `head` gets EPIPE here)
// would tell the caller the mutation failed when it succeeded — the exit code
// stays truthful about the command's own work. [LAW:no-silent-failure] the
// failure is still loud, on the channel that cannot lie about the mutation.
func printMutationSyncStalenessWarning(ctx context.Context, w io.Writer, ws workspace.Info, now time.Time) {
	rec, pushAge, pushKnown := lastPushOutcome(ws, now)
	lines := syncPushFailureLines(rec, pushAge, pushKnown, runningBinaryVersion())
	lines = append(lines, fetchStalenessLines(lastTargetFetch(ctx, ws, now))...)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			fmt.Fprintf(os.Stderr, "lit: staleness banner not written: %v\n", err)
			return
		}
	}
}
