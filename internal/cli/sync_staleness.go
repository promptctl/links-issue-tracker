package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
	"github.com/promptctl/links-issue-tracker/internal/environ"
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

// fetchSuccessMarkerPath is a remote's fetch record: its modification time
// is the moment since which local knowledge of that remote has gone
// unconfirmed, and its content says what that moment was (fetchBasis). Empty,
// it was a confirmation — a fetch of that remote succeeded, or it advertised
// no lit data or the same refs the last successful receive already brought in
// (sync_receive_ask.go). Holding fetchRecordUnconfirmed, no confirmation has
// been recorded here since lit started tracking the remote. Distinct from
// receiveMarkerPath, which marks an attempt (so its debounce still applies
// even to a remote that is failing every fetch). One file per remote, because
// a fetch is a fact about the remote it fetched: a fetch of another remote
// says nothing about the one the banner reports on. [LAW:one-source-of-truth]
func fetchSuccessMarkerPath(ws workspace.Info, remote storage.RemoteName) string {
	return perRemotePath(ws, "fetched", remote)
}

// fetchRecordUnconfirmed is the content of a fetch record lit started before
// any fetch of its remote succeeded here. An empty record is a success, as
// every record an earlier lit wrote is.
const fetchRecordUnconfirmed = "unconfirmed\n"

// fetchBasis is what a fetch record's age is measured from.
type fetchBasis int

const (
	// lastFetchSucceeded: the age is since the last confirmation. The zero
	// fetchBasis is neither, so a fetch nobody gave a basis panics in
	// warningFormat instead of claiming a success.
	lastFetchSucceeded fetchBasis = iota + 1
	// noFetchSucceeded: no confirmation is on record; the age is since lit
	// started tracking the remote.
	noFetchSucceeded
)

// warningFormat is the stale-fetch line for an age measured from b, taking
// the remote, the age and the threshold clause.
func (b fetchBasis) warningFormat() string {
	switch b {
	case lastFetchSucceeded:
		return "sync: last successful fetch from %s was %s ago (%s) — run 'lit sync fetch'"
	case noFetchSucceeded:
		return "sync: no successful fetch from %s on record in the %s since lit started tracking it (%s) — run 'lit sync fetch'"
	}
	panic(fmt.Sprintf("illegal fetchBasis %d", b))
}

// markFetchSuccess records "local knowledge of remote is current as of now".
// Callers are every real DOLT_FETCH call site that returned no error — the
// explicit `lit sync fetch`/`lit sync pull`, the reconcile command's
// pre-reconcile fetch, the automatic receive — and the automatic receive's
// unmoved and no-lit-data answers, which prove the same thing without the
// fetch. Each marks
// the remote it fetched, whichever remote the banner reports on.
//
// Truncated in place, never renamed over: Windows refuses a rename over a file
// a reader has open, and readFetchRecord holds the record open. The reader
// reads the content before the modification time, so a read racing this write
// pairs at worst the old content with the new time, below any threshold.
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

// startFetchRecord starts tracking remote: it publishes the record that says
// no confirmation of remote is on record as of now, unless any record exists —
// a success written since the caller found none wins. Durable, so a crash
// leaves no record or this whole one, never an empty file that would read as
// a success.
func startFetchRecord(ws workspace.Info, remote storage.RemoteName) error {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return err
	}
	return atomicfile.PublishFile(fetchSuccessMarkerPath(ws, remote), []byte(fetchRecordUnconfirmed), 0o644, atomicfile.Durable)
}

// readFetchRecord parses remote's fetch record. missing is true, with a nil
// fetch, only when no record exists: lit has not started tracking remote. A
// record that cannot be read, or holds content no lit this one knows wrote,
// is reported to stderr and reads as nil and not missing: it is neither a
// fact to act on nor an absence to fill.
// [LAW:parse-dont-validate] [LAW:no-silent-failure]
//
// The content is read before the modification time, on the one open file, so
// the time is never older than the content beside it (markFetchSuccess).
func readFetchRecord(stderr io.Writer, ws workspace.Info, remote storage.RemoteName, now time.Time) (fetch *remoteFetch, missing bool) {
	f, err := os.Open(fetchSuccessMarkerPath(ws, remote))
	if os.IsNotExist(err) {
		return nil, true
	}
	if err != nil {
		fmt.Fprintf(stderr, "lit: fetch record for %s unreadable: %v\n", remote, err)
		return nil, false
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, int64(len(fetchRecordUnconfirmed)+1)))
	if err != nil {
		fmt.Fprintf(stderr, "lit: fetch record for %s unreadable: %v\n", remote, err)
		return nil, false
	}
	info, err := f.Stat()
	if err != nil {
		fmt.Fprintf(stderr, "lit: fetch record for %s unreadable: %v\n", remote, err)
		return nil, false
	}
	var basis fetchBasis
	switch string(content) {
	case "":
		basis = lastFetchSucceeded
	case fetchRecordUnconfirmed:
		basis = noFetchSucceeded
	default:
		fmt.Fprintf(stderr, "lit: fetch record for %s holds content this lit does not know: %q\n", remote, content)
		return nil, false
	}
	return &remoteFetch{remote: remote, age: now.Sub(info.ModTime()), basis: basis}, false
}

// remoteFetch is one remote's fetch record, read: which remote, how long
// local knowledge of it has gone unconfirmed, and what that age is measured
// from.
type remoteFetch struct {
	remote storage.RemoteName
	age    time.Duration
	basis  fetchBasis
}

// trackTargetFetch reads the fetch record of the remote lit syncs with
// (workspaceSyncRemote), the one both staleness banners warn about, and is
// where lit starts tracking that remote: finding no record, it publishes the
// record that starts the clock and reports nil for this read. So a remote no
// fetch ever succeeded against, whether automatic receive keeps failing on it
// or nothing tries it at all, warns once the threshold passes, while a
// workspace lit has only just met gets the whole threshold first — time for
// automatic receive to run. One writer for every way into "no success on
// record" — a fresh clone, a renamed or re-pointed remote, an upgrade from
// the single legacy marker — because each is a sync remote this read finds no
// record for. [LAW:single-enforcer]
//
// nil also when there is no such remote to read: no remote configured, or
// remotes that cannot be read or resolved. The banner is best-effort, so an
// unresolved remote prints no fetch line, as it prints no freshness line;
// `lit doctor` names why. A record that cannot be started is reported, and
// the next read tries again. [LAW:no-silent-failure]
// [LAW:types-are-the-program] a known fetch always names its remote.
func trackTargetFetch(ctx context.Context, stderr io.Writer, ws workspace.Info, now time.Time) *remoteFetch {
	remote, ok, err := workspaceSyncRemote(ctx, ws, "")
	if err != nil || !ok {
		return nil
	}
	fetch, missing := readFetchRecord(stderr, ws, remote.Name(), now)
	if missing {
		if err := startFetchRecord(ws, remote.Name()); err != nil {
			fmt.Fprintf(stderr, "lit: fetch record for %s not started: %v\n", remote.Name(), err)
		}
	}
	return fetch
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
// Each basis says only what its record proves: a record lit started holds no
// success, which is not proof no fetch ever succeeded.
func fetchStalenessLines(fetch *remoteFetch) []string {
	if fetch == nil || fetch.age < unfetchedStalenessThreshold {
		return nil
	}
	return []string{fmt.Sprintf(fetch.basis.warningFormat(),
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
func printStalenessWarning(ctx context.Context, stderr io.Writer, env environ.Env, w io.Writer, ws workspace.Info, st storage.Store, now time.Time) error {
	report := resolveDoctorSyncFreshness(ctx, env, ws, st)
	fetch := trackTargetFetch(ctx, stderr, ws, now)
	// Build drift leads the whole banner. It is the deepest of the three: a
	// binary that predates master can be the reason the sync lines below read
	// the way they do, so a reader who takes it in first interprets everything
	// after it correctly. It is also the rarest, which is what makes the top
	// slot affordable.
	lines := resolveBuildStalenessLines(now)
	// The push-failure line leads the sync pair: it names the CAUSE (pushes are
	// failing), which the ahead-count line below only shows the accumulating
	// effect of.
	rec, pushAge, pushKnown := lastPushOutcome(stderr, ws, now)
	lines = append(lines, syncPushFailureLines(rec, pushAge, pushKnown, runningBinaryVersion(stderr))...)
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
// flows through, after the command's engine has closed, so it needs no
// engine: it reads storage-dir markers (push outcome, the sync remote's fetch
// record, which it starts when none exists) and git's remote config: the
// store-backed ahead
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
func printMutationSyncStalenessWarning(ctx context.Context, stderr io.Writer, env environ.Env, w io.Writer, ws workspace.Info, now time.Time) {
	rec, pushAge, pushKnown := lastPushOutcome(stderr, ws, now)
	lines := syncPushFailureLines(rec, pushAge, pushKnown, runningBinaryVersion(stderr))
	lines = append(lines, fetchStalenessLines(trackTargetFetch(ctx, stderr, ws, now))...)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			fmt.Fprintf(stderr, "lit: staleness banner not written: %v\n", err)
			return
		}
	}
}
