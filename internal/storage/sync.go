package storage

import "github.com/promptctl/links-issue-tracker/internal/merge"

// This file is the vocabulary the sync and reconcile capabilities speak. It
// lives beside the contract rather than inside an engine because a capability
// interface can only name types every engine can name.
//
// [LAW:one-source-of-truth] These types were the Dolt store's; they are now the
// contract's, and the engine re-exports the old spellings by alias so no caller
// moved. There is one declaration of each, not two.

// SyncState identifies the store's on-disk content at a point in time: where it
// lives, and a digest of what it held. It is the staleness signal a caller
// records after a sync and compares against later.
type SyncState struct {
	Path        string
	ContentHash string
}

// SyncRemote names one configured peer and where it is reached.
type SyncRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SyncStatusRow reports one unit of pending local change. What a "table" is
// belongs to the engine that reports it; the contract carries the row through
// to the renderer without interpreting it.
type SyncStatusRow struct {
	TableName string `json:"table_name"`
	Staged    bool   `json:"staged"`
	Status    string `json:"status"`
}

// SyncStatusReport is the whole local-side picture one command renders: which
// engine build is running, where the store's history stands, what is uncommitted,
// and who the peers are.
type SyncStatusReport struct {
	DoltVersion string          `json:"dolt_version"`
	Branch      string          `json:"branch"`
	HeadCommit  string          `json:"head_commit"`
	HeadMessage string          `json:"head_message"`
	Status      []SyncStatusRow `json:"status"`
	Remotes     []SyncRemote    `json:"remotes"`
}

// SyncFreshnessState classifies the local data branch's position relative to
// the remote-tracking ref. It is derived solely from whether that ref exists
// and the ahead/behind commit counts (see SyncFreshness.State), so there is one
// mapping from observation to label and no caller re-derives it.
// [LAW:one-source-of-truth]
type SyncFreshnessState string

const (
	SyncNeverSynced SyncFreshnessState = "never_synced"
	SyncUpToDate    SyncFreshnessState = "up_to_date"
	SyncAhead       SyncFreshnessState = "ahead"
	SyncBehind      SyncFreshnessState = "behind"
	SyncDiverged    SyncFreshnessState = "diverged"
)

// SyncFreshness reports the local data branch's position relative to the
// remote-tracking ref `remotes/<Remote>/<Branch>`. That ref reflects the remote
// as of the last fetch or push, so Behind is "as of last fetch" — computing it
// never contacts the network. Synced is false when the ref does not exist yet
// (the remote has never been pushed to or fetched from); Ahead and Behind are
// zero in that state, which is why State, not the raw counts, is the discriminant
// a renderer switches on.
type SyncFreshness struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Synced bool   `json:"synced"`
	Ahead  int64  `json:"ahead"`
	Behind int64  `json:"behind"`
	// OldestDivergedUnix is the Unix time (seconds) of the OLDEST commit in the
	// union of the two divergent ranges — i.e. when this fork first happened. It
	// dates the divergence itself, so an escalation surface can distinguish a
	// fresh divergence from one that has festered for days. Zero when nothing has
	// diverged (both counts are 0) or the branch never synced. It is a raw
	// timestamp, not an age: the clock that turns it into "N hours ago" lives at
	// the rendering boundary, keeping this read a pure function of local refs.
	// [LAW:effects-at-boundaries] [LAW:types-are-the-program] a stored age would
	// let the value contradict "now"; a timestamp cannot.
	OldestDivergedUnix int64 `json:"oldest_diverged_unix"`
}

// State derives the classification from the raw observations. Keeping it a
// computed method (rather than a stored field) makes a label that contradicts
// the counts unrepresentable. [LAW:types-are-the-program]
func (f SyncFreshness) State() SyncFreshnessState {
	if !f.Synced {
		return SyncNeverSynced
	}
	switch {
	case f.Ahead == 0 && f.Behind == 0:
		return SyncUpToDate
	case f.Behind == 0:
		return SyncAhead
	case f.Ahead == 0:
		return SyncBehind
	default:
		return SyncDiverged
	}
}

// SyncReceiveState classifies what a single background receive did, derived
// from the post-fetch freshness. [LAW:one-source-of-truth] One mapping from
// freshness to outcome; the CLI renders this, it never re-derives it.
type SyncReceiveState string

const (
	// SyncReceiveUpToDate: local already at the remote head; fetch found nothing.
	SyncReceiveUpToDate SyncReceiveState = "up_to_date"
	// SyncReceiveFastForwarded: local was strictly behind and advanced to the
	// remote head with no merge commit — the only state that mutates local data.
	SyncReceiveFastForwarded SyncReceiveState = "fast_forwarded"
	// SyncReceiveAhead: local has unpushed commits and the remote has nothing
	// new; there is nothing to receive (the push side delivers local commits).
	SyncReceiveAhead SyncReceiveState = "ahead"
	// SyncReceiveDiverged: both sides moved; a fast-forward is impossible and a
	// real merge is required. The background receive deliberately does NOT merge
	// here — that is the foreground, agent-present reconcile (links-multi-machine-ttde.2).
	// Reported, never silently skipped. [LAW:no-silent-failure]
	SyncReceiveDiverged SyncReceiveState = "diverged"
	// SyncReceiveNeverSynced: no remote-tracking ref even after a fetch (the
	// remote has no data on this branch yet); nothing to receive.
	SyncReceiveNeverSynced SyncReceiveState = "never_synced"
)

// SyncReceiveResult reports the receive outcome and the ahead/behind counts it
// was decided from, plus — when diverged — the Unix time the divergence began,
// so the inline reconcile that follows a SyncReceiveDiverged can date the fork
// it is about to heal without a second freshness read. Zero unless diverged.
type SyncReceiveResult struct {
	State              SyncReceiveState
	Ahead              int64
	Behind             int64
	OldestDivergedUnix int64
}

// SyncPullState classifies what a single `lit sync pull` did, derived from the
// receive (fetch + fast-forward) and, when the branch diverged, the field-aware
// reconcile. [LAW:one-source-of-truth] one mapping from the underlying
// receive/reconcile outcomes to a pull label; the CLI renders this, it never
// re-derives it.
type SyncPullState string

const (
	// SyncPullUpToDate: local already at the remote head; nothing to pull.
	SyncPullUpToDate SyncPullState = "up_to_date"
	// SyncPullFastForwarded: local was strictly behind and advanced to the
	// remote head with no merge commit.
	SyncPullFastForwarded SyncPullState = "fast_forwarded"
	// SyncPullLinearized: local diverged and the field-aware reconcile merged
	// the divergence into linear history — the same outcome the automatic
	// receive produces, reached explicitly here.
	SyncPullLinearized SyncPullState = "linearized"
	// SyncPullProsePending: local diverged and every code-owned field settled,
	// but a free-text field diverged on both sides. Nothing is committed; the
	// prose conflicts are held for the agent surface. [LAW:no-silent-failure]
	SyncPullProsePending SyncPullState = "prose_pending"
	// SyncPullUnrelated: local diverged from a remote it shares no history with, so
	// the reconcile found no common ancestor and merged nothing. Nothing is
	// committed; the divergence is surfaced for wholesale/union resolution rather
	// than merged through an absent base. [LAW:no-silent-failure]
	SyncPullUnrelated SyncPullState = "unrelated_histories"
	// SyncPullAhead: local has unpushed commits and the remote has nothing new;
	// there is nothing to pull (push delivers local commits).
	SyncPullAhead SyncPullState = "ahead"
	// SyncPullNeverSynced: the remote has no ref for this branch even after a
	// fetch — the branch has never been pushed, so there is nothing to pull.
	SyncPullNeverSynced SyncPullState = "never_synced"
)

// SyncPullResult reports the pull outcome, the ahead/behind counts it was
// decided from, and — for SyncPullProsePending only — the free-text conflicts
// held for the agent surface.
type SyncPullResult struct {
	State   SyncPullState        `json:"state"`
	Ahead   int64                `json:"ahead"`
	Behind  int64                `json:"behind"`
	Pending []merge.ProsePending `json:"pending,omitempty"`
	// OldestDivergedUnix dates the divergence this pull observed (Unix seconds),
	// so a held-conflict surface can escalate by age. Zero unless the pull met a
	// divergence. Carried from the receive that classified the freshness.
	OldestDivergedUnix int64 `json:"oldest_diverged_unix,omitempty"`
	// Unrelated carries the both-sides issue-id partition, non-nil only for
	// SyncPullUnrelated. Carried straight off the reconcile that detected the
	// no-common-ancestor divergence, so the pull surface enumerates the same
	// partition `lit sync reconcile` does. [LAW:one-source-of-truth]
	Unrelated *UnrelatedInventory `json:"unrelated,omitempty"`
}

// SyncPushResult reports what the peer said about the delivery.
type SyncPushResult struct {
	Status  int64  `json:"status"`
	Message string `json:"message"`

	// Maintenance is what the engine did to reclaim local storage while
	// servicing this push, in the engine's own words, and empty when it found
	// nothing worth reporting — so an ordinary push carries no maintenance line
	// at all. Every state a reader would act on (work performed, work declined,
	// an I/O failure) is non-empty, so nothing actionable reaches the empty
	// value. It is deliberately separate from Message: Message is the
	// engine's verbatim push output and callers render it as `raw`, so folding
	// a second subject into it would make `raw` no longer raw.
	// [LAW:one-source-of-truth] one field, one subject.
	//
	// The vocabulary is engine-neutral on purpose — an engine that keeps no
	// local caches leaves this empty rather than being obliged to describe
	// maintenance it does not perform.
	Maintenance string `json:"maintenance,omitempty"`
}

// SyncReconcileState classifies what a single foreground reconcile did with a
// diverged local branch. [LAW:one-source-of-truth] One mapping from the engine's
// outcome to a label; the CLI renders this, it never re-derives it.
type SyncReconcileState string

const (
	// SyncReconcileNotDiverged: the branch is not diverged (resolved by a push
	// race, or it never diverged). Nothing to reconcile; the caller's other
	// freshness states own those paths.
	SyncReconcileNotDiverged SyncReconcileState = "not_diverged"
	// SyncReconcileLinearized: the field-aware engine resolved every field; the
	// merged result was replayed forward onto the remote head — the folded local
	// commits landing individually with their original messages and timestamps,
	// concluded by the reconcile's marker commit — leaving linear history with no
	// merge commit, so the next push fast-forwards.
	SyncReconcileLinearized SyncReconcileState = "linearized"
	// SyncReconcileProsePending: the engine settled every code-owned field, but at
	// least one free-text field diverged on both sides. Nothing is committed and
	// the local branch is left untouched (still diverged); the prose conflicts are
	// returned for the agent surface to merge. [LAW:no-silent-failure] a divergence
	// the engine cannot resolve alone is surfaced, never auto-committed by picking
	// a side.
	SyncReconcileProsePending SyncReconcileState = "prose_pending"
	// SyncReconcileUnrelated: the local branch and the remote-tracking ref share no
	// common ancestor — independently-created stores, or one that was re-inited — so
	// there is no base for a three-way merge. The reconcile DETECTS this before any
	// write and commits nothing: the three-way path assumes a base, and driving an
	// absent one into it fails obscurely (an empty/no-row merge-base, not a clear
	// diagnosis). The divergence is real but unmergeable by the base-assuming engine;
	// it is surfaced for the wholesale/union resolution the rest of this epic builds,
	// never crashed through an empty merge-base. [LAW:no-silent-failure]
	SyncReconcileUnrelated SyncReconcileState = "unrelated_histories"
	// SyncReconcileTookLocal: the operator resolved an unrelated-history divergence by
	// taking the LOCAL side wholesale. The local backlog was replayed forward onto the
	// remote head — its commits landing individually with their original messages and
	// timestamps, concluded by the take's marker commit whose diff is the discard —
	// so it is now a fast-forwardable descendant the next push converges the remote
	// onto; the remote-only issues were discarded by design. Only SyncResolveUnrelated
	// produces this — the autonomous reconcile never picks a side.
	// [LAW:no-silent-failure] a data-dropping resolution is only ever a deliberate
	// choice, never the automatic path.
	SyncReconcileTookLocal SyncReconcileState = "took_local"
	// SyncReconcileTookRemote: the operator resolved an unrelated-history divergence by
	// taking the REMOTE side wholesale. The local branch was reset to the remote head,
	// so local content now equals the remote and sync is clean; the local-only issues
	// were discarded by design. Only SyncResolveUnrelated produces this.
	SyncReconcileTookRemote SyncReconcileState = "took_remote"
	// SyncReconcileCombined: the operator resolved an unrelated-history divergence by
	// COMBINING both sides — the union of every issue, with ids present on both field-merged
	// against an empty base (the two-way degrade of the three-way engine). The unioned
	// backlog was replayed forward onto the remote head — the folded local commits landing
	// individually with their original messages and timestamps, concluded by the combine's
	// marker commit — so local is now a fast-forwardable descendant the next push converges
	// the remote onto; NO side's unique issues were dropped. Only the combine resolution
	// produces this, and only when every
	// prose field settled — an on-both prose divergence lands SyncReconcileProsePending
	// instead, exactly as the shared-history three-way does. [LAW:no-silent-failure] the
	// union never silently drops an issue; a shared-id prose conflict is held, never picked.
	SyncReconcileCombined SyncReconcileState = "combined"
)

// SyncReconcileResult reports the reconcile outcome, the ahead/behind counts it
// was decided from, and the three commit anchors it merged. Pending is non-empty
// only for SyncReconcileProsePending.
type SyncReconcileResult struct {
	State      SyncReconcileState
	Ahead      int64
	Behind     int64
	LocalHead  string
	RemoteHead string
	BaseCommit string
	// Pending carries the free-text fields that diverged on both sides, with
	// base/ours/theirs, so the agent surface can merge intent instead of picking a
	// side. Empty unless State is SyncReconcileProsePending.
	Pending []merge.ProsePending
	// Unrelated carries the both-sides issue-id partition (only-local, only-remote,
	// on-both) so the operator can see what each side holds before choosing a
	// wholesale/union resolution. Non-nil only for SyncReconcileUnrelated; there is
	// no base to diff against, so it is read directly off the LocalHead/RemoteHead
	// anchors. [LAW:types-are-the-program] the field present names the state that
	// produced it.
	Unrelated *UnrelatedInventory
	// Replayed counts the folded side's commits that landed individually — with
	// their original message, timestamp, and author — ahead of the settling marker
	// commit. Zero for every non-mutating outcome, and for a fold whose every
	// per-commit projection was already contained in the spine (only the marker
	// landed). It is read back off the spine after the replay, so it reports what
	// actually landed, not what was attempted. [FRAMING:representation]
	Replayed int
}

// UnrelatedInventory partitions issue ids across the two sides of an
// unrelated-history divergence by which side holds each: ids only the local head
// carries, ids only the remote head carries, and ids both carry. The three slices
// are sorted and mutually disjoint by construction — every id present on either
// side lands in exactly one — so the partition is total and a consumer renders or
// resolves it without re-deriving membership. [LAW:types-are-the-program] the type
// is the "what each side holds" answer; the take-one/union resolutions later in the
// epic read the same partition rather than re-querying both heads.
type UnrelatedInventory struct {
	OnlyLocal  []string `json:"only_local,omitempty"`
	OnlyRemote []string `json:"only_remote,omitempty"`
	OnBoth     []string `json:"on_both,omitempty"`
}

// UnrelatedResolution is the whole-store choice that settles an unrelated-history
// divergence: take the local backlog wholesale, or take the remote backlog
// wholesale. It is the field-level resolve concept (internal/merge, twoTier's Tier-2
// pick between two sides that both moved) lifted from field scope to the whole store:
// with no merge-base every issue is "changed on both sides from empty", so the only
// resolution is which side to take entire. The epic's later `combine` is a third
// value of this same type, flowing through the same reconcile boundary, never a
// parallel mode. [LAW:types-are-the-program] the value carries the whole decision;
// there is no legal take with no side.
type UnrelatedResolution string

const (
	// TakeLocal keeps the local backlog and discards the remote-only issues; the
	// remote-tracking side converges to local on the next push.
	TakeLocal UnrelatedResolution = "local"
	// TakeRemote keeps the remote backlog and discards the local-only issues; local
	// content becomes equal to the remote head.
	TakeRemote UnrelatedResolution = "remote"
)

// Valid reports whether r is a resolution an engine can apply. It is the door
// guard every Reconciler runs before touching the store, so an unknown side is
// rejected loudly rather than silently no-op'd at the dispatch.
// [LAW:no-silent-failure]
//
// It lives on the type rather than inside one engine because which values are
// legal is a fact about this contract vocabulary, not about how Dolt happens to
// apply it — a second engine offering Reconciler must reject exactly the same
// set, and a re-derived copy is a copy that can drift. [LAW:single-enforcer]
func (r UnrelatedResolution) Valid() bool {
	return r == TakeLocal || r == TakeRemote
}
