package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// fetchSuccessMarkerPath is the single file whose modification time is the
// last time a fetch against the remote actually SUCCEEDED — distinct from
// receiveMarkerPath, which marks an attempt (so its debounce still applies
// even to a remote that is failing every fetch). A workspace whose fetches
// keep failing must still read as stale; conflating "attempted" with
// "succeeded" would hide exactly that. [LAW:one-source-of-truth]
func fetchSuccessMarkerPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, "fetch-success.last")
}

// markFetchSuccess records "a fetch against the remote succeeded now". Callers
// are every real DOLT_FETCH call site that returned no error: the explicit
// `lit sync fetch`/`lit sync pull`, the reconcile command's pre-reconcile
// fetch, and the inline auto-receive. [LAW:single-enforcer] one marker, every
// successful-fetch call site writes it the same way.
func markFetchSuccess(ws workspace.Info) error {
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for fetch-success marker: %w", err)
	}
	if err := os.WriteFile(fetchSuccessMarkerPath(ws), nil, 0o644); err != nil {
		return fmt.Errorf("write fetch-success marker: %w", err)
	}
	return nil
}

// lastFetchSuccessAge reports how long ago a fetch last succeeded. ok is false
// when no fetch has ever succeeded on this workspace — mirroring
// shouldReceiveNow's reading that a missing marker means "never happened",
// not "infinitely stale": a workspace that simply has not had automatic
// receive run yet is not flagged before it gets the chance to.
// [LAW:no-defensive-null-guards] absence is a real, distinct state, not a
// fabricated worst-case age. A stat error other than "does not exist" —
// permission denied, a corrupt storage dir — is a real operational failure,
// not evidence of "never fetched"; it is surfaced to stderr rather than
// silently folded into the same ok=false, so a broken storage dir is
// diagnosable instead of just reading as a quiet workspace.
// [LAW:no-silent-failure]
func lastFetchSuccessAge(ws workspace.Info, now time.Time) (age time.Duration, ok bool) {
	info, err := os.Stat(fetchSuccessMarkerPath(ws))
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "lit: fetch-success marker unreadable: %v\n", err)
		}
		return 0, false
	}
	return now.Sub(info.ModTime()), true
}

// syncStalenessLines renders zero or more prominent warning lines from an
// already-resolved sync freshness report and last-fetch age — the same silent
// drift (unpushed local changes, a remote nobody has checked in days) that let
// the field incident this epic exists to prevent go unnoticed for a week, now
// surfaced on the ordinary commands an agent actually runs instead of only on
// `lit doctor`, which nobody runs unasked. Pure over its inputs so the two
// conditions are unit-testable without a live store, mirroring
// printSyncFreshness's split from its own resolve step.
// [LAW:dataflow-not-control-flow]
//
// Scope, deliberately: this checks State() == SyncAhead, not "Ahead > 0" —
// a persistent SyncDiverged already gets the heavier `<agent-instructions>`
// SyncFailureError block from the reconcile machinery (owned by
// links-sync-pgct.4), and printing a second, lighter banner for the same root
// fact here would compete with that surface rather than add information. It
// also does not special-case SyncNeverSynced-with-unpushed-local-data — that
// requires knowing whether the local store holds anything worth protecting,
// which no cheap signal here answers; the closely related silent-fresh-store
// bug that could produce that state was fixed in links-sync-pgct.1, and this
// ticket's own contract covers only "ahead" and "fetch is stale", so a
// NeverSynced workspace is left to the second condition below (a
// never-fetched-in-threshold remote still warns) rather than a speculative
// third case.
func syncStalenessLines(report doctorSyncReport, fetchAge time.Duration, fetchAgeKnown bool) []string {
	if report.Kind != doctorSyncResolved {
		return nil
	}
	var lines []string
	f := report.Freshness
	ref := f.Remote + "/" + f.Branch
	if f.State() == storage.SyncAhead {
		lines = append(lines, fmt.Sprintf(
			"sync: %d local change(s) not pushed to %s, as of last fetch — run 'lit sync push'",
			f.Ahead, ref,
		))
	}
	lines = append(lines, fetchStalenessLines(ref, fetchAge, fetchAgeKnown)...)
	return lines
}

// fetchStalenessLines renders the stale-fetch warning. ref names the remote
// when the caller has resolved one ("origin/main"); an empty ref renders the
// same warning without it — the store-free mutation-side banner knows the
// fetch age (a marker stat) but not the remote (an engine read), and the
// warning must not be withheld for want of a name.
// [LAW:one-type-per-behavior] one renderer, the ref is data.
func fetchStalenessLines(ref string, fetchAge time.Duration, fetchAgeKnown bool) []string {
	if !fetchAgeKnown || fetchAge < unfetchedStalenessThreshold {
		return nil
	}
	from := ""
	if ref != "" {
		from = " from " + ref
	}
	return []string{fmt.Sprintf(
		"sync: last successful fetch%s was %s ago (over %s) — run 'lit sync fetch'",
		from, humanizeCoarseDuration(fetchAge), humanizeCoarseDuration(unfetchedStalenessThreshold),
	)}
}

// syncPushFailureLines renders the loud signal for a workspace whose last push
// attempt did not land — the condition links-sync-pgct.10 exists to surface.
// The ahead-count banner above cannot serve a mutating command: its own commit
// is pushed only by a mirror that runs after the process exits, so a mutating
// command is ALWAYS ahead at the moment it would print, and a banner that fires
// on every healthy mutation trains its readers to ignore it. "The last attempt
// failed" is the discriminator that separates degraded from healthy, and it
// needs no engine — exactly what a post-close, post-mutation print site can
// afford. Pure over the already-read marker state. [LAW:dataflow-not-control-flow]
func syncPushFailureLines(rec pushOutcomeRecord, age time.Duration, known bool) []string {
	if !known || !rec.failed() {
		return nil
	}
	to := ""
	if rec.Remote != "" && rec.Branch != "" {
		to = " to " + rec.Remote + "/" + rec.Branch
	}
	return []string{fmt.Sprintf(
		"sync: automatic push%s is FAILING — last attempt %s ago: %s — changes stay on this machine until a push succeeds; run 'lit sync push'",
		to, humanizeCoarseDuration(age), oneLineReason(rec.Reason),
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

// printSyncStalenessWarning resolves sync freshness and last-fetch age (the
// effects), then prints syncStalenessLines' output, one line each. A single
// call adds the whole banner to a read command — the same one-call ergonomic
// resolveBuildStatusNote gives dev-build status, and deliberately the same
// per-call-site wiring that ticket precedent used rather than a new central
// hook: each read command that wants this banner adds the call itself.
// Best-effort like that resolver: an unresolved or no-remote workspace prints
// nothing rather than aborting the caller, because this banner is
// supplementary, not itself a diagnostic. [LAW:no-silent-failure]
// [LAW:effects-at-boundaries]
func printSyncStalenessWarning(ctx context.Context, w io.Writer, ws workspace.Info, st storage.Store, now time.Time) error {
	report := resolveDoctorSyncFreshness(ctx, ws, st)
	fetchAge, fetchAgeKnown := lastFetchSuccessAge(ws, now)
	// The push-failure line leads: it names the CAUSE (pushes are failing),
	// which the ahead-count line below only shows the accumulating effect of.
	rec, pushAge, pushKnown := lastPushOutcome(ws, now)
	lines := syncPushFailureLines(rec, pushAge, pushKnown)
	lines = append(lines, syncStalenessLines(report, fetchAge, fetchAgeKnown)...)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// printMutationSyncStalenessWarning is the staleness banner for mutating
// commands — the surface links-sync-pgct.10 adds. It runs at the one dispatch
// seam every mutating command already flows through, after the command's
// engine has closed, so it reads only the storage-dir markers (push outcome,
// fetch success): the store-backed ahead count is both unavailable there and,
// per syncPushFailureLines' rationale, the wrong predicate for a mutating
// session anyway. A session that only chains mutations against a failing
// remote now hears about it on its very next command instead of never.
//
// Write failures are reported to stderr, never returned: the mutation this
// banner trails is already durable, and an exit code that flips nonzero over
// a banner write (an agent piping stdout through `head` gets EPIPE here)
// would tell the caller the mutation failed when it succeeded — the exit code
// stays truthful about the command's own work. [LAW:no-silent-failure] the
// failure is still loud, on the channel that cannot lie about the mutation.
func printMutationSyncStalenessWarning(w io.Writer, ws workspace.Info, now time.Time) {
	rec, pushAge, pushKnown := lastPushOutcome(ws, now)
	fetchAge, fetchAgeKnown := lastFetchSuccessAge(ws, now)
	lines := syncPushFailureLines(rec, pushAge, pushKnown)
	lines = append(lines, fetchStalenessLines("", fetchAge, fetchAgeKnown)...)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			fmt.Fprintf(os.Stderr, "lit: staleness banner not written: %v\n", err)
			return
		}
	}
}
