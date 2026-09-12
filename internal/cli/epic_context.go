package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// childStatus is the display state of one epic child. The variants defined in
// this file (closed/in_progress/ready/blocked) each render their own marker, so
// the render loop never branches on which state a child is in. Go interfaces
// aren't sealed — exhaustiveness here rests on locality (all variants live in
// this file), not the compiler.
// [LAW:types-are-the-program] What the compiler *does* enforce is the per-variant
// payload: a "blocked" child carries at least one blocking reason in the type,
// and closed/in_progress/ready have no field to carry one — so the
// blocked-with-no-reason state is unrepresentable and no callsite defends
// against it.
type childStatus interface {
	marker() string
}

type statusClosed struct{}

func (statusClosed) marker() string { return "[closed]" }

type statusInProgress struct{}

func (statusInProgress) marker() string { return "[in_progress]" }

type statusReady struct{}

func (statusReady) marker() string { return "[ready]" }

// statusBlocked is a child the readiness gate holds back, carrying every reason
// it holds it back FOR. The payload is head-plus-tail rather than a slice so a
// blocked child with no reason cannot be constructed: the marker's claim is
// "not startable, and here is why", and a why-less blocked marker would be the
// [ready] bug in the other direction.
//
// The marker used to read "[blocked-by <id>]", a shape that only fits a reason
// whose detail IS an id. Two of the registry's four blocking kinds have no id
// to name — a missing field, a needs-design label — so the id-shaped marker is
// gone: one phrasing (BlockingReason.Phrase) covers every kind, and the
// renderer never asks which kind it holds. [LAW:dataflow-not-control-flow]
type statusBlocked struct {
	reason BlockingReason   // the witness — a blocked child always has at least one
	more   []BlockingReason // any further reasons, in annotation order
}

func (s statusBlocked) marker() string {
	phrases := []string{s.reason.Phrase()}
	for _, reason := range s.more {
		phrases = append(phrases, reason.Phrase())
	}
	// Every reason, not the first: naming one of three invites closing it and
	// finding the child still unservable — the smaller version of the same lie
	// this marker was filed for (links-epic-context-oezb). The backlog names
	// them all too, so the two surfaces read alike.
	return "[blocked: " + strings.Join(phrases, "; ") + "]"
}

// statusFrozen is a child that has left the flow — archived or deleted. The
// sum expressed four display states where the domain has five: a deleted child
// rendered as "[ready]", inviting an agent to start a ticket every transition
// refuses. The standing word is the payload rather than the retention value so
// the marker cannot name a variant issueStanding did not produce.
type statusFrozen struct{ standing string }

func (s statusFrozen) marker() string { return "[" + s.standing + "]" }

// epicChild pairs a child issue with its already-classified display status, so
// rendering is pure formatting over resolved values.
type epicChild struct {
	Issue  model.Issue
	Status childStatus
}

// EpicContext is the fully-resolved plan slice for one epic: the epic itself
// plus its children in rank order, each pre-classified, the id of the focused
// child (empty when none), and the open blocks edges that cross the epic
// boundary. It is the seam between data resolution (buildEpicContext) and
// rendering (renderEpicContext); siblings extend this value rather than the
// renderer.
type EpicContext struct {
	Epic      model.Issue
	Children  []epicChild
	Focused   string
	CrossEpic crossEpicEdges
}

// crossEpicEdge is one direct blocks edge that crosses the epic boundary.
// Blocked is the id that is blocked; Blocker is the id that blocks. Rendering
// is identical regardless of which endpoint is inside the epic — the inside
// endpoint decides only which subsection the edge lands in, never the line
// format.
type crossEpicEdge struct {
	Blocked string
	Blocker string
}

// crossEpicEdges partitions the boundary-crossing edges by which side is
// internal. The partition is the direction: an edge's membership in a slice is
// the only discriminator the renderer needs, so the render loop never branches
// on direction.
// [LAW:types-are-the-program] Two slices rather than one slice tagged with a
// direction enum: the partition lives in the value, so the renderer cannot
// misclassify an edge and no callsite re-derives a side it was already told.
type crossEpicEdges struct {
	BlocksExternally  []crossEpicEdge // external ticket blocked by an internal one
	BlockedExternally []crossEpicEdge // internal ticket blocked by an external one
}

// statusMarkerWidth pads the fixed-form markers ([closed]/[in_progress]/[ready])
// to a common column so child titles align. Blocked markers carry reasons of
// unbounded width and intentionally overflow this column rather than pushing
// every title rightward to accommodate the longest one.
const statusMarkerWidth = len("[in_progress]")

// classifyChildStatus maps a child issue and the readiness verdict for it to a
// display status.
// [LAW:dataflow-not-control-flow] The match is over the child's discriminated
// lifecycle state; ready vs blocked is decided by the verdict value, not by
// whether some branch runs.
//
// The retention axis is read first because it dominates: an archived or deleted
// child's status describes work nobody may do, so reporting it as ready or
// blocked would answer a question the reader did not ask. [LAW:one-source-of-truth]
// The word comes from issueStanding, the one composer of the two axes.
//
// [LAW:single-enforcer] readiness is the gate's verdict, read here, never
// recomputed here. This display used to derive its own blocker list from
// `blocks` edges alone, so a child held back by any of the registry's three
// other blocking kinds — a missing required field, needs-design, an earlier
// same-lane sibling — was drawn [ready] while `lit next` refused to serve it
// (links-epic-context-oezb). IsReady is false exactly when BlockingReasons is
// non-empty, by that type's construction, so the head index below is total.
func classifyChildStatus(child model.Issue, readiness IssueReadiness) childStatus {
	if model.Frozen(child.Retention()) {
		return statusFrozen{standing: issueStanding(child)}
	}
	switch child.State() {
	case model.StateClosed:
		return statusClosed{}
	case model.StateInProgress:
		return statusInProgress{}
	}
	if readiness.IsReady() {
		return statusReady{}
	}
	reasons := readiness.BlockingReasons()
	return statusBlocked{reason: reasons[0], more: reasons[1:]}
}

// buildEpicContext resolves an epic and its children into an EpicContext.
// focusedChildID is the child the caller is "at" ("" for none, e.g. an
// epic-level call). requiredFields is the repo's ready-policy, passed as a
// value exactly as classifyWorkable takes it, so the annotation that a required
// field is missing reaches this slice too.
//
// Each child is annotated once and its relations fetched once; both feed its
// status classification and the cross-epic edge collection, so there is one
// resolved source per child rather than a separate fetch per concern.
func buildEpicContext(ctx context.Context, st storage.Store, requiredFields []string, epicID, focusedChildID string) (EpicContext, error) {
	epicRels, err := st.GetRelationsByIDs(ctx, []string{epicID})
	if err != nil {
		return EpicContext{}, err
	}
	// GetRelationsByIDs omits subjects that don't exist; the epic is the subject
	// and must resolve, so its absence is a NotFound, not a zero-value render.
	// [LAW:no-defensive-null-guards] This fails loudly at the store boundary
	// (matching the prior GetIssueDetail path) rather than skipping silently.
	epic, ok := epicRels[epicID]
	if !ok {
		return EpicContext{}, storage.NotFoundError{Entity: "issue", ID: epicID}
	}
	internal := epicMemberIDs(epic.Issue.ID, epic.Children)
	// The children go through the SAME annotator set `lit next` and `lit backlog`
	// route on, so all three surfaces answer "can this be started" from one
	// verdict. [LAW:single-enforcer] Annotate preserves input order, so the rows
	// come back in the epic-rank order epic.Children arrived in, and the batch
	// fetch inside makes the loop below pure map lookups rather than a fetch per
	// child. [LAW:dataflow-not-control-flow]
	// The focus scope is dropped rather than applied: the epic plan is the epic's
	// own child list, and narrowing it to the focus path would print a partial
	// plan that still reads as the whole one. [LAW:no-silent-failure]
	annotated, childRels, _, err := annotateIssues(ctx, st, requiredFields, epic.Children)
	if err != nil {
		return EpicContext{}, err
	}
	children := make([]epicChild, 0, len(annotated))
	var cross crossEpicEdges
	// [LAW:one-source-of-truth] "Inside the epic" is one boundary used two ways:
	// epicMemberIDs excludes intra-epic edges, and collect gathers the crossing
	// ones. The epic node is a member, so its own external edges cross the
	// boundary exactly as a child's do — collect from the epic too, or the two
	// uses of "inside" would disagree.
	cross.collect(epic, internal)
	for _, row := range annotated {
		// A child listed as an epic member but absent from the batch is a data
		// inconsistency, not a row to fabricate — fail loudly rather than append
		// a zero-value Issue. [LAW:no-defensive-null-guards]
		childRel, ok := childRels[row.ID]
		if !ok {
			return EpicContext{}, storage.NotFoundError{Entity: "issue", ID: row.ID}
		}
		// The lifecycle marker and the readiness verdict describe row.Issue, the
		// one value the annotators read, so the two halves of a child's status
		// can never be drawn from two different snapshots of it.
		// [LAW:one-source-of-truth]
		children = append(children, epicChild{
			Issue:  row.Issue,
			Status: classifyChildStatus(row.Issue, ClassifyReadiness(row.Annotations)),
		})
		cross.collect(childRel, internal)
	}
	cross.sortByEndpoints()
	return EpicContext{Epic: epic.Issue, Children: children, Focused: focusedChildID, CrossEpic: cross}, nil
}

// epicTarget names the epic whose plan context `lit show` appends for an issue,
// and the child to mark focused within it ("" for none). It is the resolved
// answer to "which plan slice does this issue belong to" — a value, so the show
// path renders unconditionally on its presence rather than re-deriving the
// cases at the callsite.
type epicTarget struct {
	EpicID  string
	Focused string
}

// epicViewFor classifies an issue into the epic plan it belongs to. A container
// (epic) shows its own children with no focus; a leaf under an epic shows that
// epic's plan with itself focused; an issue in no epic returns nil — the genuine
// "no plan slice" case, encoded as absence rather than an empty value.
// [LAW:types-are-the-program] The optionality is the value: nil means no block,
// so the show path never re-tests the three cases. The container-parent test is
// the same predicate enrichWithParentEpic uses, so "what counts as an epic
// parent" has one definition. [LAW:one-source-of-truth]
func epicViewFor(issue model.Issue, parent *model.Issue) *epicTarget {
	if issue.IsContainer() {
		return &epicTarget{EpicID: issue.ID}
	}
	if parent != nil && parent.IsContainer() {
		return &epicTarget{EpicID: parent.ID, Focused: issue.ID}
	}
	return nil
}

// resolveEpicContext resolves the plan slice one shown issue belongs to, or nil
// when it belongs to none. It is the show path's entire store-and-config stage,
// which is what lets the config read be conditional on need: epicViewFor is pure
// over the detail already in hand, so the required-fields policy — a config.Load
// off disk that also validates unrelated settings — is read only once a real
// plan slice is known to want it. An issue in no epic reads no repo config at
// all, so a plain `lit show` keeps its independence from config it never uses.
// [LAW:effects-at-boundaries]
//
// The policy still reaches buildEpicContext as a value rather than as an
// *app.App it could load from, for the reason classifyWorkable states: the
// policy is repo config, the rest of that path is store data, and keeping them
// apart is what lets a plain store drive the builder. [LAW:locality-or-seam]
func resolveEpicContext(ctx context.Context, ap *app.App, detail model.IssueDetail) (*EpicContext, error) {
	// [LAW:no-defensive-null-guards] target is an explicit optional: nil is the
	// real "no epic membership" case, not a defended-against bug.
	target := epicViewFor(detail.Issue, detail.Parent)
	if target == nil {
		return nil, nil
	}
	requiredFields, err := readyRequiredFields(ap)
	if err != nil {
		return nil, err
	}
	ec, err := buildEpicContext(ctx, ap.Store, requiredFields, target.EpicID, target.Focused)
	if err != nil {
		return nil, err
	}
	return &ec, nil
}

// writeEpicContext appends the epic plan block for one shown issue. A leading
// blank line separates the block from the issue body; an issue in no epic — a
// nil context — writes nothing.
//
// Rendering is split from resolution so the show path can fail before printing
// the body. A body written ahead of a resolution error is shaped exactly like
// the legitimate "this ticket has no epic" output, so a reader holding only
// stdout cannot tell an absent plan slice from one that could not be computed.
// [LAW:parse-dont-validate]
func writeEpicContext(w io.Writer, ec *EpicContext) error {
	if ec == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, "\n%s", renderEpicContext(*ec))
	return err
}

// epicMemberIDs is the set of ids inside the epic — the epic node itself plus
// its children — which is the membership test that decides whether a blocks
// edge crosses the boundary. The epic id is included so an edge between a child
// and its own epic is intra-epic, never surfaced as a cross-epic dependency.
func epicMemberIDs(epicID string, children []model.Issue) map[string]struct{} {
	set := make(map[string]struct{}, len(children)+1)
	set[epicID] = struct{}{}
	for _, child := range children {
		set[child.ID] = struct{}{}
	}
	return set
}

// collect appends the boundary-crossing blocks edges incident to one epic
// member — the epic node or any of its children. A member that has left the
// flow carries no live plan context, so it contributes nothing; same-epic
// counterparts are excluded because their ordering is already conveyed by rank
// in the children list, and out-of-flow counterparts are dropped by
// inPlayExcluding.
// [LAW:one-source-of-truth] Both the member test and the counterpart filter ask
// InPlay, so the two ends of an edge answer "still live" the same way.
func (x *crossEpicEdges) collect(member storage.IssueRelations, internal map[string]struct{}) {
	if !member.Issue.InPlay() {
		return
	}
	id := member.Issue.ID
	for _, blocker := range inPlayExcluding(member.DependsOn, internal) {
		x.BlockedExternally = append(x.BlockedExternally, crossEpicEdge{Blocked: id, Blocker: blocker.ID})
	}
	for _, dependent := range inPlayExcluding(member.Blocks, internal) {
		x.BlocksExternally = append(x.BlocksExternally, crossEpicEdge{Blocked: dependent.ID, Blocker: id})
	}
}

// inPlayExcluding keeps the issues that are still in play and whose ids are not
// in excluded. A nil excluded set drops nothing by membership, leaving the plain
// in-play filter; passing the epic member ids (epic plus children) drops
// same-epic counterparts so only boundary-crossing ones remain.
// [LAW:one-source-of-truth] InPlay, not State() != StateClosed: an archived or
// deleted counterpart has left the flow exactly as a closed one has, and the
// name says which question is asked so the old spelling cannot creep back.
func inPlayExcluding(others []model.Issue, excluded map[string]struct{}) []model.Issue {
	var out []model.Issue
	for _, other := range others {
		if _, skip := excluded[other.ID]; skip {
			continue
		}
		if other.InPlay() {
			out = append(out, other)
		}
	}
	return out
}

// sortByEndpoints orders each subsection by (blocked, blocker) so render output
// is deterministic regardless of child iteration order. (blocked, blocker) is a
// total order over distinct edges — no two compare equal — so determinism comes
// from the comparator, not from sort stability.
func (x *crossEpicEdges) sortByEndpoints() {
	byEndpoints := func(edges []crossEpicEdge) {
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].Blocked != edges[j].Blocked {
				return edges[i].Blocked < edges[j].Blocked
			}
			return edges[i].Blocker < edges[j].Blocker
		})
	}
	byEndpoints(x.BlocksExternally)
	byEndpoints(x.BlockedExternally)
}

// empty reports whether no boundary-crossing edges exist in either direction —
// the single value test that decides whether the section renders at all.
func (x crossEpicEdges) empty() bool {
	return len(x.BlocksExternally) == 0 && len(x.BlockedExternally) == 0
}

// renderEpicContext renders an EpicContext as a plain-text block: the epic id,
// title, and "why" (first line of its description), followed by each child in
// rank order with its status marker. The focused child is marked "▶ ... (you
// are here)".
func renderEpicContext(ec EpicContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Epic: %s — %s\n", ec.Epic.ID, ec.Epic.Title)
	fmt.Fprintf(&b, "Why: %s\n", firstLine(ec.Epic.Description))
	b.WriteString("\nChildren:\n")
	b.WriteString(renderChildren(ec.Children, ec.Focused))
	b.WriteString(renderCrossEpic(ec.CrossEpic))
	return b.String()
}

// renderChildren renders the children block: the rank-ordered rows, or
// "(none)" when the epic has none. The empty case is a property of the list,
// not a branch the caller has to remember.
func renderChildren(children []epicChild, focused string) string {
	if len(children) == 0 {
		return "  (none)\n"
	}
	var b strings.Builder
	for _, child := range children {
		b.WriteString(renderChildLine(child, child.Issue.ID == focused))
	}
	return b.String()
}

// renderCrossEpic renders the "Cross-epic dependencies" section: the two
// direction subsections, each omitted when its slice is empty, and the whole
// section omitted when no edges cross in either direction. Both subsections
// share one line format, so the only thing that varies per subsection is its
// header and which slice it lists — never the rendering of a line.
func renderCrossEpic(x crossEpicEdges) string {
	if x.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nCross-epic dependencies:\n")
	b.WriteString(renderCrossSubsection("Blocks externally", x.BlocksExternally))
	b.WriteString(renderCrossSubsection("Blocked externally", x.BlockedExternally))
	return b.String()
}

// renderCrossSubsection renders one direction's edges under a header, or
// nothing when the slice is empty. The line format is identical for both
// directions because a cross-epic edge always reads "<blocked> blocked by
// <blocker>" — the direction lives in which slice supplied the edges.
func renderCrossSubsection(header string, edges []crossEpicEdge) string {
	if len(edges) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", header)
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s blocked by %s\n", e.Blocked, e.Blocker)
	}
	return b.String()
}

// renderChildLine renders one child row. The focused row gets a "▶" gutter and
// a trailing "(you are here)"; both gutters occupy the same width so titles stay
// aligned.
func renderChildLine(child epicChild, focused bool) string {
	prefix, suffix := "    ", ""
	if focused {
		prefix, suffix = "  ▶ ", "   (you are here)"
	}
	return fmt.Sprintf("%s%-*s %s  %s%s%s\n", prefix, statusMarkerWidth, child.Status.marker(), child.Issue.ID, child.Issue.Title, laneTag(child.Issue.Lane), suffix)
}

// laneTag renders a child's lane as an inline tag so the epic plan shows which
// sub-sequence each child belongs to (shared lane = serialized; distinct lane =
// parallel). The empty lane — the fully-sequential default — renders as nothing,
// so a lane-free epic looks exactly as it did before lanes existed.
// [LAW:dataflow-not-control-flow] The tag is a pure function of the lane value;
// the empty case is data rendering to empty, not a branch the caller manages.
func laneTag(lane string) string {
	if lane == "" {
		return ""
	}
	return "  [lane: " + lane + "]"
}

// firstLine returns the first non-blank line of s as prose: surrounding
// whitespace and any leading markdown heading hashes are stripped, because epic
// descriptions conventionally open with a "# Heading" the "why" should read
// past.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
