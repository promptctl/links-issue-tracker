package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// This file is the owner's out-of-band channel (links-sync-pgct.4): when sync
// detects a real divergence or a failing push, the OWNER — the human whose work
// the backlog carries — hears about it the day it happens, through a hook they
// configured (sync.owner_notify_cmd), not seven days later through an agent's
// archaeology. The agent-facing blocks stay the in-band surface; this is the
// copy that leaves the terminal.

const (
	// ownerNotifyHookTimeout bounds the hook run so a hung notifier (an
	// unreachable ntfy host) cannot hang the lit command that detected the event.
	ownerNotifyHookTimeout = 10 * time.Second
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

// ownerNotifyKind is the closed set of owner-relevant degraded states. The three
// divergence kinds ARE the sync-failure divergence classes — one vocabulary, so
// the trace, the failure block, and the notification can never spell the same
// condition two ways — plus the push kind, whose detection point (a completed
// push attempt) has no SyncFailure. [LAW:one-source-of-truth]
type ownerNotifyKind string

const ownerNotifyPushFailed ownerNotifyKind = "push_failed"

// ownerNotifyDivergenceKinds is the group cleared together when a divergence
// episode ends: any converged state (fast-forwarded, linearized, combined,
// taken, not-diverged) resolves all three, whichever was notified.
var ownerNotifyDivergenceKinds = []ownerNotifyKind{
	ownerNotifyKind(syncFailureProseHeld),
	ownerNotifyKind(syncFailureDivergedUnresolved),
	ownerNotifyKind(syncFailureUnrelatedHistories),
}

// ownerNotifyEvent is one owner-relevant occurrence: what degraded (Kind), the
// domain sentence describing it (Summary), and which sync target it concerns.
type ownerNotifyEvent struct {
	Kind    ownerNotifyKind
	Summary string
	Remote  string
	Branch  string
}

// fingerprint identifies the episode the marker de-duplicates. For a
// divergence, the same kind against the same sync target is the same ongoing
// fact — a re-pointed remote is a NEW fork and notifies immediately. A push
// failure keys on the kind alone: the same broken channel can fail at
// different stages (a refs check that resolves no remote, a push the remote
// refuses), and per-stage target detail would make one outage read as several
// episodes and ping the owner once per flap.
func (ev ownerNotifyEvent) fingerprint() string {
	if ev.Kind == ownerNotifyPushFailed {
		return string(ev.Kind)
	}
	return string(ev.Kind) + " " + ev.Remote + "/" + ev.Branch
}

// ownerNotifyEventForFailure maps a surfaced sync failure to its owner
// notification, or ok=false for the classes that are not divergences (a
// remote-schema-ahead block is a version condition, not the ticket's "real
// divergence" set). [LAW:dataflow-not-control-flow] every surface calls this
// unconditionally; the class value decides.
func ownerNotifyEventForFailure(failure SyncFailure) (ownerNotifyEvent, bool) {
	switch failure.Class {
	case syncFailureProseHeld, syncFailureDivergedUnresolved, syncFailureUnrelatedHistories:
		return ownerNotifyEvent{
			Kind:    ownerNotifyKind(failure.Class),
			Summary: failure.whatLine(),
			Remote:  failure.Remote,
			Branch:  failure.Branch,
		}, true
	default:
		return ownerNotifyEvent{}, false
	}
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
func maybeNotifyOwner(ctx context.Context, ws workspace.Info, ev ownerNotifyEvent) {
	if isTruthyEnv(os.Getenv(DisableAutoSyncEnvVar)) {
		return
	}
	markerPath := ownerNotifyMarkerPath(ws, ev.Kind)
	if !ownerNotifyDue(markerPath, ev.fingerprint(), time.Now()) {
		return
	}
	cfg, err := config.Load(pathspec.New(ws.RootDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: owner notification skipped, config unreadable: %v\n", err)
		return
	}
	hook := strings.TrimSpace(cfg.Sync.OwnerNotifyCmd)
	if hook == "" {
		return
	}
	metadata := map[string]string{"remote": ev.Remote, "sync_branch": ev.Branch}
	if err := runOwnerNotifyHook(ctx, hook, ws.RootDir, ev); err != nil {
		fmt.Fprintf(os.Stderr, "lit: owner notification hook failed (retries on the next detection): %v\n", err)
		recordSyncTraceLogged(ws, syncTraceRecord{
			Command:   ownerNotifyTraceCommand,
			Decision:  string(ev.Kind),
			Status:    "error",
			Reason:    err.Error(),
			BuildNote: resolveBuildStatusNote(time.Now()),
			Metadata:  metadata,
		})
		return
	}
	if err := writeMarkerAtomic(ws, markerPath, []byte(ev.fingerprint()+"\n")); err != nil {
		fmt.Fprintf(os.Stderr, "lit: owner-notify marker not written: %v\n", err)
	}
	recordSyncTraceLogged(ws, syncTraceRecord{
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
func runOwnerNotifyHook(ctx context.Context, hook, repoRoot string, ev ownerNotifyEvent) error {
	hookCtx, cancel := context.WithTimeout(ctx, ownerNotifyHookTimeout)
	defer cancel()
	cmd := exec.CommandContext(hookCtx, "sh", "-c", hook)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"LIT_NOTIFY_KIND="+string(ev.Kind),
		"LIT_NOTIFY_SUMMARY="+ev.Summary,
		"LIT_NOTIFY_REMOTE="+ev.Remote,
		"LIT_NOTIFY_BRANCH="+ev.Branch,
		"LIT_NOTIFY_REPO="+repoRoot,
	)
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
func clearOwnerNotify(ws workspace.Info, kinds ...ownerNotifyKind) {
	for _, kind := range kinds {
		if err := os.Remove(ownerNotifyMarkerPath(ws, kind)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "lit: owner-notify marker not cleared: %v\n", err)
		}
	}
}

// observePushOutcomeForOwner is the push half of the owner channel, fed by the
// same completion record the push-outcome marker persists so the two can never
// disagree about how the attempt ended. [LAW:one-source-of-truth] A landed push
// ends the push episode; a failed attempt notifies — except a deliberate
// cancellation (ctrl-C mid-push), which is an operator abandoning the attempt,
// not the remote degrading.
func observePushOutcomeForOwner(ctx context.Context, ws workspace.Info, rec pushOutcomeRecord, attemptErr, pushErr error) {
	switch {
	case rec.Decision == pushDecisionPushed:
		clearOwnerNotify(ws, ownerNotifyPushFailed)
	case rec.failed():
		if errors.Is(attemptErr, context.Canceled) || errors.Is(pushErr, context.Canceled) {
			return
		}
		maybeNotifyOwner(ctx, ws, ownerNotifyEvent{
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
