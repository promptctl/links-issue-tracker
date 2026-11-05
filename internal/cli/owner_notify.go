package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// This file is the owner's out-of-band channel: when sync detects a real
// divergence, a schema refusal that stops the receive, or a failing push, the
// OWNER — the human whose work the backlog carries — hears about it the day it
// happens, through a hook they configured (sync.owner_notify_cmd), not days
// later through an agent's archaeology. The agent-facing blocks stay the
// in-band surface; this is the copy that leaves the terminal.

const (
	// ownerNotifyHookTimeout bounds the hook run so a hung notifier (an
	// unreachable ntfy host) cannot hang the lit command that detected the event.
	ownerNotifyHookTimeout = 10 * time.Second

	// ownerNotifyPipeWaitDelay is how long past the kill Wait will hold the
	// output pipe. Named rather than inlined because it is a term in the
	// parent's post-spawn tail, which the detached mirror's parent-wait bound
	// is derived from — a number only spelled at its use site cannot be summed
	// there. [LAW:one-source-of-truth]
	ownerNotifyPipeWaitDelay = time.Second
	// ownerNotifyCooldown is how often a PERSISTING condition re-notifies. The
	// first detection of an episode always fires (its marker was cleared when the
	// previous episode resolved); the cooldown only paces repeats of the same
	// ongoing fact, so a divergence that festers pings daily instead of on every
	// command.
	ownerNotifyCooldown = 24 * time.Hour
	// ownerNotifyTraceCommand labels the durable sync-trace record of each send
	// attempt. Deliberately not a "lit ..." string: this is a subsystem decision,
	// not a runnable command, and the trace must not pretend otherwise.
	// [FRAMING:representation]
	ownerNotifyTraceCommand = "owner-notify"
)

// ownerNotifyKind is the closed set of owner-relevant degraded states. The
// kinds ARE the sync-failure classes — the divergence classes, the unsettled
// receive, and the schema refusals, whichever half of the channel detected
// them — one vocabulary, so the trace, the failure block, and the notification
// can never spell the same condition two ways; plus push_failed, for a push
// that failed with no sync-failure class (the remote unreachable, a deadline,
// a mirror that could not start). [LAW:one-source-of-truth]
type ownerNotifyKind string

const ownerNotifyPushFailed ownerNotifyKind = "push_failed"

// The two groups below are the enders: a kind pages only if some outcome ends
// its episode, and the mapping reads these lists, so a class cannot page and
// never clear, or clear and never page. [LAW:one-source-of-truth]

// ownerNotifySettleEndedKinds is the group one settle ends: any converged state
// (fast-forwarded, linearized, combined, taken, not-diverged) resolves a
// divergence, and the receive half's two schema refusals are raised only
// inside the reconcile's replay of a diverged plan, so every one of those
// states proves them gone as well, whichever was notified.
var ownerNotifySettleEndedKinds = []ownerNotifyKind{
	ownerNotifyKind(syncFailureProseHeld),
	ownerNotifyKind(syncFailureDivergedUnresolved),
	ownerNotifyKind(syncFailureUnrelatedHistories),
	ownerNotifyKind(syncFailureIDCollision),
	ownerNotifyKind(syncFailureRemoteSchemaAhead),
	ownerNotifyKind(syncFailureSchemaCeilingLift),
}

// ownerNotifyPushEndedKinds is the group one landed push ends: push_failed,
// and every schema refusal the push's guard raises (a remote schema ahead of
// this binary, a commit above the ceiling, a raise from no release, an unheld
// lowering). A push that landed passed each of those guards, so it is proof
// each is gone. A remote schema ahead is in both groups: the push and the
// reconcile's replay each detect it, and either a landed push or a settle
// ends it. A lift refused by the ceiling is receive-side, and a landed push
// says nothing about it.
var ownerNotifyPushEndedKinds = []ownerNotifyKind{
	ownerNotifyPushFailed,
	ownerNotifyKind(syncFailureRemoteSchemaAhead),
	ownerNotifyKind(syncFailureSchemaCeiling),
	ownerNotifyKind(syncFailureSchemaRaiseRequiresRelease),
	ownerNotifyKind(syncFailureSchemaLoweringRequiresHold),
}

// ownerNotifyEvent is one owner-relevant occurrence: what degraded (Kind), the
// domain sentence describing it (Summary), which sync target it concerns, and,
// for an unsettled receive, when its run of stops began (EpisodeStart, zero for
// every other kind).
type ownerNotifyEvent struct {
	Kind         ownerNotifyKind
	Summary      string
	Remote       string
	Branch       string
	EpisodeStart time.Time
}

// fingerprint identifies the episode the marker de-duplicates. For a
// divergence, the same kind against the same sync target is the same ongoing
// fact — a re-pointed remote is a NEW fork and notifies immediately. A push
// failure keys on the kind alone: the same broken channel can fail at
// different stages (a refs check that resolves no remote, a push the remote
// refuses), and per-stage target detail would make one outage read as several
// episodes and ping the owner once per flap. An unsettled receive keys on when
// its run of stops began: the run IS the episode, so a run that a settle ended
// and a new one started reads as a new fingerprint and pages on its first
// detection, with no marker to clear on the way. [LAW:one-source-of-truth] the
// episode is derived from the run's record, never kept beside it.
func (ev ownerNotifyEvent) fingerprint() string {
	switch ev.Kind {
	case ownerNotifyPushFailed:
		return string(ev.Kind)
	case ownerNotifyKind(syncFailureReceiveUnsettled):
		return string(ev.Kind) + " " + strconv.FormatInt(ev.EpisodeStart.Unix(), 10)
	default:
		return string(ev.Kind) + " " + ev.Remote + "/" + ev.Branch
	}
}

// ownerNotifyEventForFailure maps a surfaced sync failure to its owner
// notification: the divergence classes, an unsettled receive, and every
// schema refusal notify — a checkout that cannot settle against the remote,
// or write to it, is off the shared backlog, and only the owner, who wrote
// the hold or cut the release, can clear that. The kind is the class,
// whichever half of the channel detected it, so the one condition both halves
// detect (a remote schema ahead of this binary: the push's guard and the
// reconcile's replay each refuse it) pages under one kind with one marker.
// [LAW:single-enforcer] one mapping from class to kind, for both halves.
// A push refused over a peer's commits and a lost forced-push lease are
// ok=false: they are the push channel failing to deliver, not a condition
// with its own remedy, and the push half pages them as push_failed.
// [LAW:dataflow-not-control-flow] every surface calls this unconditionally;
// the class value decides.
func ownerNotifyEventForFailure(failure SyncFailure) (ownerNotifyEvent, bool) {
	kind := ownerNotifyKind(failure.Class)
	switch {
	case slices.Contains(ownerNotifySettleEndedKinds, kind) || slices.Contains(ownerNotifyPushEndedKinds, kind):
		return ownerNotifyEvent{
			Kind:    kind,
			Summary: failure.whatLine(),
			Remote:  failure.Remote,
			Branch:  failure.Branch,
		}, true
	case failure.Class == syncFailureReceiveUnsettled:
		return ownerNotifyEvent{
			Kind:         kind,
			Summary:      failure.whatLine(),
			Remote:       failure.Remote,
			Branch:       failure.Branch,
			EpisodeStart: failure.UnsettledSince,
		}, true
	default:
		return ownerNotifyEvent{}, false
	}
}

// surfaceSyncFailure is what a foreground sync command does with the error its
// engine call returned: the error renders as the sync-failure contract
// (asSyncFailure), and its class pages the owner, since an explicit `lit sync
// pull` or `lit sync reconcile` is the first — on a checkout with no automatic
// receive, the only — detector of a schema refusal.
// [LAW:dataflow-not-control-flow] every error passes through; the class decides.
// [LAW:single-enforcer] the five foreground error paths share this one point.
func surfaceSyncFailure(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, err error) error {
	err = asSyncFailure(err)
	var failure SyncFailureError
	if errors.As(err, &failure) {
		notifyOwnerForFailure(ctx, stderr, env, ws, failure.Failure)
	}
	return err
}

// notifyOwnerForFailure is the one detection point for a surface that holds
// a sync failure — the automatic receive, a held pull or reconcile, doctor, a
// foreground engine error, a push the schema refused — and the class decides
// whether the owner hears. The only classes that do not map are the two the
// push half pages as push_failed, and that half hands only schema refusals
// here, so a class arriving unmapped is reported as the bug it is rather than
// passing as a refusal another half will page, which it never saw.
// [LAW:single-enforcer] [LAW:no-silent-failure]
func notifyOwnerForFailure(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, failure SyncFailure) {
	ev, ok := ownerNotifyEventForFailure(failure)
	if !ok {
		fmt.Fprintf(stderr, "lit: sync failure %q reached the owner channel with no kind of its own; the owner was not paged — this is a bug; please report it\n", failure.Class)
		return
	}
	maybeNotifyOwner(ctx, stderr, env, ws, ev)
}

// ownerNotifyMarkerPath is the per-kind sent marker: its content is the episode
// fingerprint and its modification time is when the owner was last notified.
// One file per kind keeps concurrent writers (a foreground command and the
// detached mirror) off each other's records without read-modify-write.
// [LAW:one-source-of-truth]
func ownerNotifyMarkerPath(ws workspace.Info, kind ownerNotifyKind) string {
	return filepath.Join(ws.StorageDir, "owner-notify."+string(kind)+".last")
}

// ownerNotifyDue decides whether this detection reaches the owner: yes when no
// marker exists (first detection of an episode), when the fingerprint changed
// (a different episode under the same kind), or when the cooldown elapsed (the
// same episode, persisting). now is a parameter so the decision is testable
// without aging real files.
func ownerNotifyDue(markerPath, fingerprint string, now time.Time) bool {
	info, err := os.Stat(markerPath)
	if err != nil {
		return true
	}
	payload, err := os.ReadFile(markerPath)
	if err != nil {
		return true
	}
	if strings.TrimSpace(string(payload)) != fingerprint {
		return true
	}
	return now.Sub(info.ModTime()) >= ownerNotifyCooldown
}

// maybeNotifyOwner runs the owner's configured hook for one detected event,
// de-duplicated per episode. It honors LIT_DISABLE_AUTO_SYNC — the hook is
// exactly the class of command side effect that switch exists to suppress in
// CI, sandboxes, and lit's own test suite. The sent marker is written only
// after the hook succeeds, so a failed hook (network down) retries on the next
// detection rather than silently marking the owner as informed.
// [LAW:no-silent-failure] a hook failure is loud on stderr and in the durable
// sync trace; [LAW:effects-at-boundaries] the config read and the process spawn
// live here, behind one named boundary, and every caller stays a pure
// "detected this" report.
func maybeNotifyOwner(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, ev ownerNotifyEvent) {
	if autoSyncDisabled(env) {
		return
	}
	markerPath := ownerNotifyMarkerPath(ws, ev.Kind)
	if !ownerNotifyDue(markerPath, ev.fingerprint(), time.Now()) {
		return
	}
	cfg, err := config.Load(env, ws.Project)
	if err != nil {
		fmt.Fprintf(stderr, "lit: owner notification skipped, config unreadable: %v\n", err)
		return
	}
	hook := strings.TrimSpace(cfg.Sync.OwnerNotifyCmd)
	if hook == "" {
		return
	}
	metadata := map[string]string{"remote": ev.Remote, "sync_branch": ev.Branch}
	if err := runOwnerNotifyHook(ctx, env, hook, ws.RootDir, ev); err != nil {
		fmt.Fprintf(stderr, "lit: owner notification hook failed (retries on the next detection): %v\n", err)
		recordSyncTraceLogged(stderr, env, ws, syncTraceRecord{
			Command:   ownerNotifyTraceCommand,
			Decision:  string(ev.Kind),
			Status:    "error",
			Reason:    err.Error(),
			BuildNote: resolveBuildStatusNote(time.Now()),
			Metadata:  metadata,
		})
		return
	}
	if err := writeMarkerAtomic(markerPath, []byte(ev.fingerprint()+"\n")); err != nil {
		fmt.Fprintf(stderr, "lit: owner-notify marker not written: %v\n", err)
	}
	recordSyncTraceLogged(stderr, env, ws, syncTraceRecord{
		Command:   ownerNotifyTraceCommand,
		Decision:  string(ev.Kind),
		Status:    "ok",
		Reason:    ev.Summary,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  metadata,
	})
}

// runOwnerNotifyHook executes the configured command through the shell with the
// event's facts in the environment, time-boxed. The hook's own output is not
// lit's contract: it is surfaced only inside the error when the hook fails.
func runOwnerNotifyHook(ctx context.Context, env environ.Env, hook, repoRoot string, ev ownerNotifyEvent) error {
	hookCtx, cancel := context.WithTimeout(ctx, ownerNotifyHookTimeout)
	defer cancel()
	cmd := env.With(
		"LIT_NOTIFY_KIND="+string(ev.Kind),
		"LIT_NOTIFY_SUMMARY="+ev.Summary,
		"LIT_NOTIFY_REMOTE="+ev.Remote,
		"LIT_NOTIFY_BRANCH="+ev.Branch,
		"LIT_NOTIFY_REPO="+repoRoot,
	).CommandContext(hookCtx, repoRoot, "sh", "-c", hook)
	// The deadline kills the sh, but CombinedOutput also waits for the output
	// pipe to close — a hook's backgrounded child inheriting stdout would hold
	// the caller past the cap indefinitely. WaitDelay forces Wait to abandon
	// the pipe shortly after the kill, making the timeout the real bound.
	// [LAW:no-ambient-temporal-coupling]
	cmd.WaitDelay = ownerNotifyPipeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}

// clearOwnerNotify ends the given kinds' episodes: the next detection of any of
// them is a NEW fact and notifies immediately, however recently the previous
// episode pinged. Callers invoke it from the outcomes that mean "converged" or
// "pushed", so the marker always represents "the owner has been told about the
// CURRENT episode". [FRAMING:representation]
func clearOwnerNotify(stderr io.Writer, ws workspace.Info, kinds ...ownerNotifyKind) {
	for _, kind := range kinds {
		if err := os.Remove(ownerNotifyMarkerPath(ws, kind)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "lit: owner-notify marker not cleared: %v\n", err)
		}
	}
}

// observePushOutcomeForOwner is the push half of the owner channel, fed by the
// same completion record the push-outcome marker persists so the two can never
// disagree about how the attempt ended, and by the schema refusal the attempt's
// push error classifies as, parsed once at the completion seam (refused is
// whether it was one). [LAW:one-source-of-truth] A landed push ends every
// push-ended episode; a failed attempt notifies — under the refusal's own
// class when the schema refused the push, since that condition has its own
// remedy and the receive half pages it under the same kind, else as
// push_failed. A deliberate cancellation (an operator abandoning the attempt,
// not the remote degrading) and a legitimately busy workspace never reach the
// notify arms at all: pushOutcomeOf classifies them as their own non-failed
// decisions, so this function needs no error inspection of its own — the
// record IS the classification. [LAW:single-enforcer] [LAW:parse-dont-validate]
func observePushOutcomeForOwner(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, rec pushOutcomeRecord, refusal SyncFailure, refused bool) {
	switch {
	case rec.Decision == pushDecisionPushed:
		clearOwnerNotify(stderr, ws, ownerNotifyPushEndedKinds...)
	case rec.failed() && refused:
		notifyOwnerForFailure(ctx, stderr, env, ws, refusal)
	case rec.failed():
		maybeNotifyOwner(ctx, stderr, env, ws, ownerNotifyEvent{
			Kind:    ownerNotifyPushFailed,
			Summary: fmt.Sprintf("a lit sync push to %s failed: %s — local ticket changes are not reaching the shared backlog.", pushTarget(rec.Remote, rec.Branch), rec.Reason),
			Remote:  rec.Remote,
			Branch:  rec.Branch,
		})
	}
}

// pushTarget names the push destination for prose, keeping an unresolved
// remote/branch an explicit phrase rather than a dangling "/".
// [LAW:no-silent-failure]
func pushTarget(remote, branch string) string {
	switch {
	case remote == "":
		return "the configured remote"
	case branch == "":
		return remote
	default:
		return remote + "/" + branch
	}
}
