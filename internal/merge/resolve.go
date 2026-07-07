package merge

import (
	"sort"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// ProseField names a free-text issue field whose concurrent rewrites only a
// reader can merge — the engine never picks a winner for these. [LAW:decomposition]
// The organizing cut is "combine vs. choose": prose is combinable only
// semantically, so it is the one class handed to the calling agent.
type ProseField string

const (
	ProseTitle       ProseField = "title"
	ProseDescription ProseField = "description"
	ProsePrompt      ProseField = "agent_prompt"
)

// ProsePending is one free-text field that diverged on both sides and was held
// back for the calling agent's semantic merge. The three versions let the agent
// preserve BOTH intents instead of discarding one.
type ProsePending struct {
	IssueID string     `json:"issue_id"`
	Field   ProseField `json:"field"`
	Base    string     `json:"base"`
	Ours    string     `json:"ours"`
	Theirs  string     `json:"theirs"`
}

// IssueResolution is the output of ResolveIssue: the merged row with every
// code-resolvable field already settled, plus the prose fields that still need
// the agent. The merged row is not a public field: it is reachable only through
// Settled (gated) or Provisional (explicitly named), so a caller cannot extract
// a committable row while ignoring unresolved prose. [LAW:types-are-the-program]
// The unsafe state — autonomously committing a provisionally-chosen prose value —
// is made unrepresentable rather than left to a discipline a caller might skip.
type IssueResolution struct {
	merged  model.Issue
	Pending []ProsePending
}

// Settled returns the merged row to commit autonomously, and ok=true ONLY when
// no prose field needs the agent. A non-empty Pending set returns ok=false, so
// the autonomous-commit path cannot adopt a provisional prose value by accident.
// [LAW:no-silent-failure] the gate is the return value, not a convention.
func (r IssueResolution) Settled() (model.Issue, bool) {
	return r.merged, len(r.Pending) == 0
}

// Provisional returns the merged row carrying provisional prose values, for the
// reconcile boundary that persists the code-resolved fields while holding the
// Pending prose for the agent surface. The name marks the callsite as the place
// that has accepted responsibility for the unresolved prose. [LAW:effects-at-boundaries]
// blocking the actual commit is the reconcile sibling's job; this only hands it
// the row, explicitly.
func (r IssueResolution) Provisional() model.Issue {
	return r.merged
}

// ResolveIssue is the deterministic core of multi-machine reconcile: given the
// three-way state of ONE ticket (base = merge-base row, ours = local, theirs =
// remote) it returns the merged row plus the prose fields that diverged. It is
// pure — no IO, no Dolt — so every policy is provable by value against
// hand-written triples.
//
// base is nil when the same id was created independently on both sides (no
// merge-base); every field is then treated as "both changed" from empty.
// oursWS/theirsWS are the two workspaces' ids; the tiebreak compares THEM, never
// "ours vs theirs", so both machines compute the same winner regardless of which
// side each calls its own. [LAW:no-ambient-temporal-coupling] Causality comes
// from the merge-base, not a clock: a field only one side moved is taken from
// that side (Tier 1), which is what makes reopen converge with no timestamp.
func ResolveIssue(base, ours, theirs *model.Issue, oursWS, theirsWS string) IssueResolution {
	r := resolver{oursWS: oursWS, theirsWS: theirsWS, hasBase: base != nil}
	if r.hasBase {
		r.base = *base
	}
	r.ours = *ours
	r.theirs = *theirs
	r.id = ours.ID

	mergedType := twoTier(r.hasBase, r.base.IssueType, ours.IssueType, theirs.IssueType, typedTiebreak[model.IssueType](&r))

	// [LAW:types-are-the-program] Build the merged row on whichever side already
	// carries the resolved type's lifecycle shape, so a leaf stays a leaf and a
	// container stays a container without synthesizing a lifecycle from nothing.
	basis := ours
	if mergedType != ours.IssueType && mergedType == theirs.IssueType {
		basis = theirs
	}
	merged := *basis

	merged.IssueType = mergedType
	merged.Title = r.prose(ProseTitle, r.base.Title, ours.Title, theirs.Title)
	merged.Description = r.prose(ProseDescription, r.base.Description, ours.Description, theirs.Description)
	merged.Prompt = r.prose(ProsePrompt, r.base.Prompt, ours.Prompt, theirs.Prompt)
	merged.Priority = twoTier(r.hasBase, r.base.Priority, ours.Priority, theirs.Priority, higher)
	merged.Topic = twoTier(r.hasBase, r.base.Topic, ours.Topic, theirs.Topic, r.tiebreak)
	merged.Lane = twoTier(r.hasBase, r.base.Lane, ours.Lane, theirs.Lane, r.tiebreak)
	merged.Rank = twoTier(r.hasBase, r.base.Rank, ours.Rank, theirs.Rank, r.tiebreak)
	merged.Labels = mergeLabelNames(r.hasBase, r.base.Labels, ours.Labels, theirs.Labels)

	// [LAW:one-source-of-truth] id/created_at are immutable; the archive/delete
	// timestamps are DERIVED, slaved to the resolved archive/delete state, never
	// merged independently.
	merged.ID = r.id
	if r.hasBase {
		merged.CreatedAt = r.base.CreatedAt
	} else {
		merged.CreatedAt = earliest(ours.CreatedAt, theirs.CreatedAt)
	}
	merged.UpdatedAt = latest(ours.UpdatedAt, theirs.UpdatedAt)
	// Retention merges on its two-timestamp wire projection: the per-flag
	// two-tier rule is the three-way's unit, and the fold back through the one
	// decoder resolves a cross-workspace archive/delete race the way every read
	// boundary does — deletion dominates. [LAW:one-source-of-truth] The sum stays
	// the domain representation; merge touches only its derived encoding.
	baseArchivedAt, baseDeletedAt := model.RetentionTimestamps(r.base.Retention())
	oursArchivedAt, oursDeletedAt := model.RetentionTimestamps(ours.Retention())
	theirsArchivedAt, theirsDeletedAt := model.RetentionTimestamps(theirs.Retention())
	merged.SetRetention(model.RetentionFromTimestamps(
		r.derivedFlagTime(boolBase(baseArchivedAt, r.hasBase), oursArchivedAt, theirsArchivedAt),
		r.derivedFlagTime(boolBase(baseDeletedAt, r.hasBase), oursDeletedAt, theirsDeletedAt),
	))

	// Status/assignee/closed_at live in the lifecycle and exist only for leaves;
	// a container's state is derived from its children and is never merged here.
	if !mergedType.IsContainer() {
		merged = r.resolveStatus(merged)
	}

	return IssueResolution{merged: merged, Pending: r.pending}
}

type resolver struct {
	id               string
	base             model.Issue
	ours             model.Issue
	theirs           model.Issue
	hasBase          bool
	oursWS, theirsWS string
	pending          []ProsePending
}

// twoTier is the one merge primitive: Tier 1 takes whichever single side moved
// the field off base (this is what makes reopen converge with no clock); Tier 2,
// reached only when BOTH sides moved to different values, defers to the field's
// policy. [LAW:dataflow-not-control-flow] The per-field variability lives in the
// tier2 value, not in branching copied per field. Go forbids type-parameterized
// methods, so hasBase is threaded explicitly rather than read off the resolver.
func twoTier[T comparable](hasBase bool, base, ours, theirs T, tier2 func(ours, theirs T) T) T {
	oursChanged := !hasBase || ours != base
	theirsChanged := !hasBase || theirs != base
	switch {
	case oursChanged && !theirsChanged:
		return ours
	case theirsChanged && !oursChanged:
		return theirs
	case !oursChanged && !theirsChanged:
		return ours
	default:
		return tier2(ours, theirs)
	}
}

// prose resolves one free-text field. Tier 1 takes the lone mover with no agent
// involvement; Tier 2 (both moved to different text) records a ProsePending and
// returns ours as a PROVISIONAL value — the non-empty Pending set is what stops
// that value from being committed.
func (r *resolver) prose(field ProseField, base, ours, theirs string) string {
	return twoTier(r.hasBase, base, ours, theirs, func(ours, theirs string) string {
		if ours == theirs {
			return ours
		}
		r.pending = append(r.pending, ProsePending{IssueID: r.id, Field: field, Base: base, Ours: ours, Theirs: theirs})
		return ours
	})
}

// resolveStatus settles the leaf lifecycle fields and re-hydrates the row.
// status uses the dominant-state join (closed > in_progress > open); assignee is
// a symmetric tiebreak; closed_at is DERIVED from the resolved status.
func (r *resolver) resolveStatus(merged model.Issue) model.Issue {
	// Status/assignee live in the lifecycle, whose accessors require a hydrated
	// issue. base is the zero value when there is no merge-base, so read those
	// fields only when it actually exists; without a base every field is "changed"
	// and the base operand is unused anyway.
	baseState := 0
	baseAssignee := ""
	if r.hasBase {
		baseState = stateRank(r.base.StatusValue())
		baseAssignee = r.base.AssigneeValue()
	}
	state := stateFromRank(twoTier(r.hasBase, baseState, stateRank(r.ours.StatusValue()), stateRank(r.theirs.StatusValue()), higher))
	// Assignee is an issue-level field resolved independently of state — it is not
	// part of the lifecycle, so it travels on merged.Assignee, not the StatusView.
	merged.Assignee = twoTier(r.hasBase, baseAssignee, r.ours.AssigneeValue(), r.theirs.AssigneeValue(), r.tiebreak)

	var closedAt *time.Time
	var resolution *model.Resolution
	var redirectTarget *string
	if state == model.StateClosed {
		closedAt = earliestTime(r.ours.ClosedAtValue(), r.theirs.ClosedAtValue())
		// The close payload (resolution + its redirect target) is settled only
		// when the merged state is closed. The symmetric tiebreak makes both
		// machines pick the same winner when each side closed with a different
		// payload; a side with no resolution (a `done` or legacy close)
		// contributes the empty payload, which loses to any real one. The target
		// travels WITH its resolution as one atom — mixing one side's resolution
		// with the other side's target could fabricate a close nobody performed.
		// [LAW:one-source-of-truth]
		resolution, redirectTarget = resolveClosePayload(r.ours, r.theirs, r.tiebreak)
	}

	hydrated, err := model.HydrateStatus(merged, model.StatusView{Value: state, ClosedAt: closedAt, Resolution: resolution, RedirectTarget: redirectTarget})
	if err != nil {
		// HydrateStatus never errors for a leaf StatusView; surface loudly
		// rather than silently keep an unmerged status. [LAW:no-silent-failure]
		panic(err)
	}
	return hydrated
}

// tiebreak is the symmetric chooser: it compares the two WORKSPACE ids so both
// machines pick the same winner regardless of which side each calls its own.
// Equal workspaces (defensive) fall back to comparing the values themselves.
func (r *resolver) tiebreak(ours, theirs string) string {
	if r.oursWS != r.theirsWS {
		if r.oursWS > r.theirsWS {
			return ours
		}
		return theirs
	}
	if ours >= theirs {
		return ours
	}
	return theirs
}

// typedTiebreak lifts the symmetric chooser to a named string type: the
// contract is symmetric selection, not string-ness, so the adapter changes the
// element type and nothing else.
func typedTiebreak[T ~string](r *resolver) func(T, T) T {
	return func(ours, theirs T) T {
		return T(r.tiebreak(string(ours), string(theirs)))
	}
}

// resolveClosePayload settles the closed-state why-not payload — the
// resolution together with the redirect target that redirecting resolutions
// carry. The two fields are one atom: whichever side's resolution wins
// contributes its own target, so the merge can never pair one side's
// resolution with the other side's target. Absent on a side is the empty
// payload; a real resolution always beats absence, and two different real
// payloads go to the symmetric tiebreak (resolution first, then target as the
// second key when the resolutions agree) so both machines pick the same
// winner. Returns (nil, nil) when neither side carries a resolution (a
// `done`/legacy close on both); the target⟹redirecting-resolution invariant
// is re-sealed by NewStatus at hydration regardless.
func resolveClosePayload(ours, theirs model.Issue, tiebreak func(ours, theirs string) string) (*model.Resolution, *string) {
	o := closePayloadOf(ours)
	t := closePayloadOf(theirs)
	var winner closePayload
	switch {
	case o == t:
		winner = o
	case o.resolution == "":
		winner = t
	case t.resolution == "":
		winner = o
	case o.resolution != t.resolution:
		if tiebreak(o.resolution, t.resolution) == o.resolution {
			winner = o
		} else {
			winner = t
		}
	// Same resolution, different targets (one possibly absent): a real target
	// beats absence; two real targets tiebreak symmetrically.
	case o.target == "":
		winner = t
	case t.target == "":
		winner = o
	default:
		winner = o
		if tiebreak(o.target, t.target) != o.target {
			winner = t
		}
	}
	if winner.resolution == "" {
		return nil, nil
	}
	resolution := model.Resolution(winner.resolution)
	var target *string
	if winner.target != "" {
		target = &winner.target
	}
	return &resolution, target
}

// closePayload is the comparable projection of one side's close payload;
// empty strings encode absence so the whole payload is one comparable value
// (o == t is what makes the atom cheap to settle). The projection's invariant
// — a target only ever accompanies a resolution — is enforced by its single
// producer below, so a {resolution:"", target:"x"} value is unconstructible
// rather than merely unreachable. [LAW:types-are-the-program]
type closePayload struct {
	resolution string
	target     string
}

// closePayloadOf derives the target only under a resolution, mirroring the
// leaf type it projects (NewStatus attaches a target only beside a
// redirecting resolution), so every constructed payload is legal by
// construction.
func closePayloadOf(issue model.Issue) closePayload {
	r := issue.ResolutionValue()
	if r == nil {
		return closePayload{}
	}
	out := closePayload{resolution: string(*r)}
	if t := issue.RedirectTargetValue(); t != nil {
		out.target = *t
	}
	return out
}

// derivedFlagTime resolves an archive/delete flag by the two-tier rule on the
// boolean "is the flag set", then slaves the timestamp to the resolved flag: set
// -> earliest non-nil time, cleared -> nil. A boolean can never reach a real
// Tier 2 (both sides moving from base land on the same value), so the tier2 OR
// is only ever a formality.
func (r *resolver) derivedFlagTime(base bool, ours, theirs *time.Time) *time.Time {
	set := twoTier(r.hasBase, base, ours != nil, theirs != nil, presentOr)
	if !set {
		return nil
	}
	return earliestTime(ours, theirs)
}

// higher is the Tier-2 dominant-state join: the larger value on a fixed total
// order (priority: urgent>normal; status via rank: closed>in_progress>open).
func higher(ours, theirs int) int {
	if ours >= theirs {
		return ours
	}
	return theirs
}

func stateRank(state string) int {
	switch model.State(state) {
	case model.StateClosed:
		return 2
	case model.StateInProgress:
		return 1
	default:
		return 0
	}
}

func stateFromRank(rank int) model.State {
	switch rank {
	case 2:
		return model.StateClosed
	case 1:
		return model.StateInProgress
	default:
		return model.StateOpen
	}
}

func boolBase(t *time.Time, hasBase bool) bool {
	return hasBase && t != nil
}

// mergeLabelNames converges a label SET by applying the per-element two-tier
// rule to each name's membership: a label one side removed (and the other left
// untouched) stays removed instead of being resurrected by a naive union, while
// a label either side added is kept. presentOr is the Tier-2 join — both adding
// or both removing agree, so it only ever formalizes that agreement.
// [LAW:single-enforcer] Set convergence is twoTier per element, the same rule
// every scalar field uses, not a second label-specific policy.
func mergeLabelNames(hasBase bool, base, ours, theirs []string) []string {
	baseSet, oursSet, theirsSet := nameSet(base), nameSet(ours), nameSet(theirs)
	out := make([]string, 0, len(oursSet)+len(theirsSet))
	for name := range unionNameSet(baseSet, oursSet, theirsSet) {
		_, inBase := baseSet[name]
		_, inOurs := oursSet[name]
		_, inTheirs := theirsSet[name]
		if twoTier(hasBase, inBase, inOurs, inTheirs, presentOr) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func presentOr(ours, theirs bool) bool { return ours || theirs }

func nameSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

func unionNameSet(sets ...map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for _, set := range sets {
		for name := range set {
			out[name] = struct{}{}
		}
	}
	return out
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earliestTime(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return cloneTimePtr(b)
	case b == nil:
		return cloneTimePtr(a)
	case a.Before(*b):
		return cloneTimePtr(a)
	default:
		return cloneTimePtr(b)
	}
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	clone := *t
	return &clone
}
