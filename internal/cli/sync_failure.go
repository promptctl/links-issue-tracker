package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// syncFailureClass is the closed set of non-transient sync failures that reach an
// agent after the field-aware engine has tried and could not converge on its own.
// Each class fixes its own remediation, so the "how to resolve" lines are chosen
// by the class value, never by the call site. [LAW:dataflow-not-control-flow]
type syncFailureClass string

const (
	// syncFailureProseHeld: the merge settled every code-owned field, but one or
	// more free-text fields diverged on both sides. Only a semantic merge resolves
	// it; the engine deliberately holds rather than pick a side. Remedy: the
	// reconcile surface, which shows base/ours/theirs.
	syncFailureProseHeld syncFailureClass = "prose_held"
	// syncFailureDivergedUnresolved: the local backlog is diverged and a
	// reconcile has not converged it — either the automatic reconcile hit a
	// backend error (Cause populated) or the divergence has simply sat unresolved
	// (doctor's view, Cause empty). Remedy: a foreground pull.
	syncFailureDivergedUnresolved syncFailureClass = "diverged_unresolved"
	// syncFailureRemoteSchemaAhead: the remote head is at a schema version this
	// binary cannot produce, so no sync write may author a commit below it. Unlike
	// a divergence, this never clears by retrying — it clears only by upgrading the
	// binary. Remedy: `lit upgrade` to the producer that advanced the remote.
	syncFailureRemoteSchemaAhead syncFailureClass = "remote_schema_ahead"
	// syncFailureUnrelatedHistories: the local backlog and the remote share no
	// common ancestor (independently-created or re-inited stores), so there is no
	// base for a three-way merge and the field-aware reconcile cannot combine them.
	// Like a schema-ahead block and unlike an ordinary divergence, this never clears
	// by retrying — only a deliberate choice resolves it: take one side's backlog
	// wholesale or union the two. Remedy: escalate the choice (the wholesale/union
	// resolution surface is not yet built).
	syncFailureUnrelatedHistories syncFailureClass = "unrelated_histories"
)

// persistentDivergenceAge and persistentDivergenceCommits are the thresholds past
// which a divergence stops reading as "reconcile in progress" and becomes an
// incident an agent must resolve now. Either signal alone trips it: a slow drip
// of a few commits over days is as much an incident as a burst of many in an
// hour. The 2026-07-08 incident was BOTH (≈5 days, 41+5 commits), and age is the
// signal a commit-count threshold alone would miss.
const (
	persistentDivergenceAge     = 24 * time.Hour
	persistentDivergenceCommits = 10
)

// syncFailureMustNotIgnore is the constant directive every block opens with. The
// severity below varies with the divergence's values; the standing instruction
// not to route around a sync failure does not. It is phrased in the imperative
// register agents act on, because in the 2026-07-08 incident an agent read a
// softer "will retry" line for two days and classified it as ambient noise.
const syncFailureMustNotIgnore = "MUST NOT IGNORE: this is not ambient noise. Do NOT classify it as a known quirk, retry past it, or route around it. Resolve it now — or explicitly surface it to the user as blocking — before continuing ticket work."

// SyncFailure is the domain state of one non-transient sync failure, independent
// of where it surfaced. It is the single input to the one contract renderer, so
// every reporter — the inline auto-reconcile, `lit sync pull`, `lit doctor` —
// produces the identical machine-stable block from the same fields. It mirrors
// store.UnsupportedSchemaVersionError: the value carries the state, and which
// lines appear is decided by which fields are populated, not by the caller.
// [LAW:single-enforcer] [LAW:types-are-the-program]
type SyncFailure struct {
	Class  syncFailureClass
	Remote string
	Branch string
	Ahead  int64
	Behind int64
	// Age is the divergence's age, computed at the surfacing boundary from the
	// store's oldest-divergent-commit timestamp. Zero means unknown, and the
	// escalation then leans on the commit counts alone.
	Age time.Duration
	// Fields carries the held free-text conflicts, populated only for
	// syncFailureProseHeld — it names WHICH fields need the agent's merge.
	Fields []merge.ProsePending
	// Cause is the backend error that prevented convergence, populated only for a
	// syncFailureDivergedUnresolved that arose from a reconcile hard-failure. It
	// renders as a trailing cause line, never as the headline. [LAW:no-silent-failure]
	// the backend detail is preserved; it is just demoted below the directive so
	// it can no longer read as the whole (ignorable) message.
	Cause error
	// RemoteSchemaVersion, LocalSupportedMax, and RemoteProducer are populated only
	// for syncFailureRemoteSchemaAhead: the remote head's applied schema version,
	// this binary's registry max, and the producer binary version to upgrade to
	// (empty when the remote head names none). [LAW:types-are-the-program] the
	// fields present name which class rendered, so a consumer cannot read a
	// remote-schema-ahead block without the versions that make it actionable.
	RemoteSchemaVersion int64
	LocalSupportedMax   int64
	RemoteProducer      string
	// Inventory carries the both-sides issue-id partition (only-local, only-remote,
	// on-both), populated only for syncFailureUnrelatedHistories. Before choosing to
	// take one side wholesale or union the two, the operator must see what each side
	// holds; this is that visibility, rendered as its own section of the block.
	// [LAW:types-are-the-program] the field present names the class that produced it.
	Inventory *store.UnrelatedInventory
}

// remoteSchemaAheadFailure builds the sync-failure contract for a store
// *RemoteSchemaAheadError, or ok=false when err is not one. It is the ONE adapter
// from the store's typed remote-ahead refusal to the CLI's one contract renderer,
// so every surface that can hit it — an explicit push/pull/reconcile, the inline
// auto-reconcile, the on-change mirror push — produces the identical block.
// [LAW:single-enforcer] [LAW:one-source-of-truth] the version fields come straight
// off the typed error; no message text is parsed.
func remoteSchemaAheadFailure(err error) (SyncFailure, bool) {
	var ahead *store.RemoteSchemaAheadError
	if !errors.As(err, &ahead) {
		return SyncFailure{}, false
	}
	return SyncFailure{
		Class:               syncFailureRemoteSchemaAhead,
		Remote:              ahead.Remote,
		Branch:              ahead.Branch,
		RemoteSchemaVersion: ahead.RemoteVersion,
		LocalSupportedMax:   ahead.BinarySupportedMax,
		RemoteProducer:      ahead.RemoteProducerVersion,
	}, true
}

// asSyncFailure converts a store *RemoteSchemaAheadError into the returnable
// sync-failure contract (so the command exits ExitConflict and prints the block),
// or returns err unchanged. The explicit sync commands route their store error
// through this one adapter so none of them renders the raw refusal instead of the
// contract. [LAW:single-enforcer]
func asSyncFailure(err error) error {
	if failure, ok := remoteSchemaAheadFailure(err); ok {
		return SyncFailureError{Failure: failure}
	}
	return err
}

// persistent reports whether the divergence has crossed from "reconcile still in
// progress" into "incident". Severity is a function of the divergence's VALUES —
// its age and its commit span — not of which surface observed it, so every
// surface agrees on when the same divergence became an incident.
// [LAW:dataflow-not-control-flow]
func (f SyncFailure) persistent() bool {
	agedOut := f.Age >= persistentDivergenceAge
	spanOut := f.Ahead+f.Behind > persistentDivergenceCommits
	return agedOut || spanOut
}

// SyncFailureError makes a SyncFailure returnable from a command: its Error() is
// the full contract block, so the top-level error sink prints the same block a
// passive surface prints inline, and ExitCode maps it to the conflict exit. One
// value, one rendering, whether it flows out as an error or is printed directly.
// [LAW:single-enforcer]
type SyncFailureError struct {
	Failure SyncFailure
}

func (e SyncFailureError) Error() string {
	return e.Failure.blockString()
}

// blockString renders the one authoritative sync-failure block: an
// <agent-instructions> envelope carrying the four contract elements — the
// directive, what happened, how to resolve, and the value-driven escalation —
// with the backend cause, when present, as a trailing line and never the
// headline. The same operations run every call; the class and the populated
// fields decide the content. [LAW:dataflow-not-control-flow]
func (f SyncFailure) blockString() string {
	var b strings.Builder
	b.WriteString("<agent-instructions>\n")
	b.WriteString("lit sync could not resolve a backlog divergence automatically and needs you.\n\n")

	// (1) Directive — constant, unmissable, class-independent.
	b.WriteString(syncFailureMustNotIgnore)
	b.WriteString("\n\n")

	// (2) What is wrong, in domain terms — the backend string is not the headline.
	fmt.Fprintf(&b, "WHAT HAPPENED: %s\n\n", f.whatLine())

	// (2b) What each side holds — the both-sides partition, present only for the
	// unrelated-histories class. The loop runs every call; inventoryLines yields
	// nothing for the other classes, so this adds no branch. [LAW:dataflow-not-control-flow]
	for _, line := range f.inventoryLines() {
		fmt.Fprintf(&b, "%s\n", line)
	}

	// (3) How to resolve — the exact commands, in order, for this class.
	b.WriteString("HOW TO RESOLVE (run in order):\n")
	for _, step := range f.resolutionSteps() {
		fmt.Fprintf(&b, "  %s\n", step)
	}
	b.WriteString("\n")

	// (4) Escalation — selected by the divergence's age and span.
	fmt.Fprintf(&b, "%s\n", f.escalationLine())

	if f.Cause != nil {
		fmt.Fprintf(&b, "\ncause (backend detail, for diagnosis only — the steps above are the fix): %v\n", f.Cause)
	}
	b.WriteString("</agent-instructions>")
	return b.String()
}

// whatLine states the failure in domain terms — schema/commit reality, not the
// raw backend error. [FRAMING:representation] the message is the representation
// of the failure; a domain sentence is a truer map than a driver string.
func (f SyncFailure) whatLine() string {
	ref := f.Remote + "/" + f.Branch
	switch f.Class {
	case syncFailureProseHeld:
		return fmt.Sprintf(
			"the field-aware merge with %s settled every code-owned field, but %s diverged on both sides — a semantic conflict only you can merge (the engine will not pick a side, so this will NOT clear on its own).",
			ref, describeHeldFields(f.Fields))
	case syncFailureDivergedUnresolved:
		// "has not been reconciled automatically" is true for both producers of this
		// class — an inline reconcile that ran and hit a backend error (Cause set),
		// and doctor observing a divergence with no reconcile attempted (Cause empty,
		// e.g. auto-sync disabled). Phrasing it as "the automatic reconcile has not
		// converged it" would imply an attempt that the doctor path never made.
		// [FRAMING:representation] the trailing cause line still names the backend
		// error when a reconcile did fail.
		return fmt.Sprintf(
			"the local backlog is diverged from %s — %d local commit(s) not yet sent and %d remote commit(s) not yet merged — and it has not been reconciled automatically.",
			ref, f.Ahead, f.Behind)
	case syncFailureRemoteSchemaAhead:
		return fmt.Sprintf(
			"%s has advanced to schema version %d, but this lit binary supports only through version %d. Pushing or reconciling from here would author a commit BELOW the remote's schema — regressing the shared backlog and dropping every field the newer schema added — so this binary will not write to %s until it is upgraded.",
			ref, f.RemoteSchemaVersion, f.LocalSupportedMax, ref)
	case syncFailureUnrelatedHistories:
		return fmt.Sprintf(
			"the local backlog and %s share no common history — they were created independently, or one was re-initialized — so there is no shared ancestor to merge against. The field-aware reconcile combines a divergence relative to a common base; with no base it cannot merge these automatically, and it committed nothing rather than pick a side. Keeping both backlogs requires taking one side wholesale or unioning them.",
			ref)
	default:
		// A class this renderer does not know must not render as a bland,
		// authoritative-looking line. Name it as a bug the way the pull payload
		// renderer surfaces an unknown state. [LAW:no-silent-failure]
		return fmt.Sprintf("an unrecognized sync-failure class %q on %s — this is a bug; please report it.", f.Class, ref)
	}
}

// resolutionSteps is the ordered command list for the class, each with a short
// gloss of what it does. The remedy lives in the tool's output, not in an agent's
// memory: in the incident the repair knowledge existed only in a session note,
// which drifts. [LAW:one-source-of-truth] the tool that detects the state names
// the fix for that state.
func (f SyncFailure) resolutionSteps() []string {
	switch f.Class {
	case syncFailureProseHeld:
		return []string{
			"lit sync reconcile        # shows base/ours/theirs for each held field and how to merge them inline",
		}
	case syncFailureDivergedUnresolved:
		return []string{
			"lit sync pull             # runs the field-aware reconcile in the foreground and reports what it settled",
			"lit sync reconcile        # only if the pull reports a held text conflict — then merge it inline",
		}
	case syncFailureRemoteSchemaAhead:
		// [LAW:dataflow-not-control-flow] the producer field selects whether the step
		// names a concrete `--to` target or the generic upgrade; the remedy — install
		// a newer binary — is the same either way. This is the REMOTE counterpart to
		// the `lit upgrade --to <producer>` line the schema-ahead LOCAL refusal emits.
		if f.RemoteProducer != "" {
			return []string{
				fmt.Sprintf("lit upgrade --to %s   # install the binary that advanced the remote to schema v%d, then retry", f.RemoteProducer, f.RemoteSchemaVersion),
			}
		}
		return []string{
			fmt.Sprintf("lit upgrade               # install a newer lit that supports schema v%d, then retry (the remote head names no producer version to target)", f.RemoteSchemaVersion),
		}
	case syncFailureUnrelatedHistories:
		// All three resolutions now exist: the two wholesale takes (destructive of the
		// OTHER side's unique issues — a deliberate choice, never run blindly; the WHAT
		// EACH SIDE HOLDS section shows exactly what each loses) and combine, the union
		// that KEEPS every issue (shared ids field-merged, an on-both prose conflict held
		// for inline resolution). combine is listed first as the keep-everything default;
		// a take is the answer only when one side should genuinely win wholesale.
		// [LAW:no-silent-failure] every named command is real and present.
		return []string{
			"lit sync reconcile combine       # KEEP BOTH: union every issue (shared ids field-merged; a prose conflict is held for you to resolve inline), then push",
			"lit sync reconcile take remote   # adopt their backlog wholesale (discards your local-only issues)",
			"lit sync reconcile take local    # keep your backlog wholesale (discards their remote-only issues), then push",
			"lit sync reconcile               # re-shows what each side holds (only-local, only-remote, on-both)",
		}
	default:
		return []string{"lit doctor                # unrecognized sync-failure class; report this"}
	}
}

// escalationLine selects the severity sentence from the divergence's values. The
// operations are identical every call; only the branch taken varies, and it
// varies on data (persistent), not on the surface that called in.
// [LAW:dataflow-not-control-flow]
func (f SyncFailure) escalationLine() string {
	// A remote-schema-ahead block is not a divergence that ages toward incident —
	// it will NOT clear on the next command or with time, only with an upgrade. So
	// its escalation is fixed by the class, not derived from age/span, which would
	// otherwise read as "recent, still routine" and invite the very wait-and-retry
	// the epic kills. [LAW:dataflow-not-control-flow] the class selects the line.
	if f.Class == syncFailureRemoteSchemaAhead {
		return "ESCALATION — BLOCKED: this will not clear by waiting or retrying; the binary must be upgraded first. Treat the workspace as blocked for writes to the remote until you run the upgrade above, or surface it to the user as blocking."
	}
	// Unrelated histories, like a schema-ahead remote, never merge on a retry — there
	// is no base and never will be one for this pair. So its escalation is fixed by
	// the class, not derived from age/span, which would otherwise read as "recent,
	// still routine" and invite the retry that can never work. [LAW:dataflow-not-control-flow]
	if f.Class == syncFailureUnrelatedHistories {
		return "ESCALATION — BLOCKED: unrelated histories never merge automatically and will not clear by retrying. Resolving requires a deliberate choice — take one side's backlog wholesale, or union the two — which lit cannot make for you. Surface it to the user as blocking before continuing ticket work."
	}
	span := f.Ahead + f.Behind
	if f.persistent() {
		return fmt.Sprintf(
			"ESCALATION — INCIDENT: this divergence has persisted for %s across %d commit(s) — far past a transient hiccup. Treat the workspace as blocked on it: resolve it now or surface it to the user as blocking. Do NOT keep doing ticket work around it.",
			f.agePhrase(), span)
	}
	return fmt.Sprintf(
		"ESCALATION — recent (%s, %d commit(s)): still within the window where a divergence is routine. Resolve it with the steps above; if it is still here on your next command it is no longer routine — escalate then.",
		f.agePhrase(), span)
}

// agePhrase humanizes the divergence age for the escalation sentence. Zero age
// (the store had no timestamp) reads as an explicit unknown rather than a
// misleading "0 seconds". [LAW:no-silent-failure]
func (f SyncFailure) agePhrase() string {
	switch {
	case f.Age <= 0:
		return "an unknown duration"
	case f.Age >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(f.Age/(24*time.Hour)))
	case f.Age >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(f.Age/time.Hour))
	case f.Age >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(f.Age/time.Minute))
	default:
		return "under a minute"
	}
}

// inventoryLines renders the both-sides issue-id partition as its own labeled
// section — what only local holds, what only the remote holds, and what both carry
// — so the operator sees the concrete inventory before choosing take-one or union.
// It returns nil for every class but unrelated-histories (and for a defensively
// absent inventory), so the renderer emits nothing for them; the trailing blank
// element separates the section from the resolution steps below.
// [FRAMING:representation] the partition is the map of what each side holds.
func (f SyncFailure) inventoryLines() []string {
	if f.Inventory == nil {
		return nil
	}
	return []string{
		"WHAT EACH SIDE HOLDS (issue ids):",
		"  only on local:  " + describeIDSet(f.Inventory.OnlyLocal),
		"  only on remote: " + describeIDSet(f.Inventory.OnlyRemote),
		"  on both:        " + describeIDSet(f.Inventory.OnBoth),
		"",
	}
}

// describeIDSet renders one partition slice as its count and members, so an empty
// side reads as an explicit "(0)" rather than a blank the reader must interpret.
// [LAW:no-silent-failure] the absence of ids on a side is stated, not left blank.
func describeIDSet(ids []string) string {
	if len(ids) == 0 {
		return "(0)"
	}
	return fmt.Sprintf("(%d): %s", len(ids), strings.Join(ids, ", "))
}

// describeHeldFields names the held free-text fields for the WHAT line, so the
// agent sees exactly which fields it owns before it opens the reconcile surface.
func describeHeldFields(pending []merge.ProsePending) string {
	ordered := merge.SortPending(pending)
	names := make([]string, 0, len(ordered))
	for _, p := range ordered {
		names = append(names, fmt.Sprintf("%s·%s", p.IssueID, p.Field))
	}
	switch len(names) {
	case 0:
		return "one or more free-text fields"
	case 1:
		return "the free-text field " + names[0]
	default:
		return fmt.Sprintf("%d free-text fields (%s)", len(names), strings.Join(names, ", "))
	}
}

// ageFromOldestDivergedUnix turns the store's oldest-divergent-commit timestamp
// into a divergence age against now. This is the one place the clock meets the
// stored fact — the store returns a timestamp, the boundary makes it an age — so
// the age can never be stored stale. [LAW:effects-at-boundaries] A zero or future
// timestamp yields zero (unknown), which the renderer states honestly.
func ageFromOldestDivergedUnix(oldestUnix int64, now time.Time) time.Duration {
	if oldestUnix <= 0 {
		return 0
	}
	age := now.Sub(time.Unix(oldestUnix, 0))
	if age < 0 {
		return 0
	}
	return age
}
