package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// childStatus is the display state of one epic child. The variants defined in
// this file (closed/in_progress/ready/blocked) each render their own marker, so
// the render loop never branches on which state a child is in. Go interfaces
// aren't sealed — exhaustiveness here rests on locality (all variants live in
// this file), not the compiler.
// [LAW:types-are-the-program] What the compiler *does* enforce is the per-variant
// payload: a "blocked" child carries its blocker id in the type, and
// closed/in_progress/ready have no field to carry one — so the
// blocked-with-no-blocker state is unrepresentable and no callsite defends
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

type statusBlocked struct{ blocker string }

func (s statusBlocked) marker() string { return "[blocked-by " + s.blocker + "]" }

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
// to a common column so child titles align. blocked-by markers carry an issue
// id of unbounded width and intentionally overflow this column rather than
// pushing every title rightward to accommodate the longest id.
const statusMarkerWidth = len("[in_progress]")

// classifyChildStatus maps a child issue and its open-blocker ids to a display
// status. openBlockers is the child's open blocker ids in a deterministic
// order; the first entry names the blocker in a blocked status.
// [LAW:dataflow-not-control-flow] The match is over the child's discriminated
// lifecycle state; open vs blocked is decided by the blocker-count value, not
// by whether some branch runs.
func classifyChildStatus(child model.Issue, openBlockers []string) childStatus {
	switch child.State() {
	case model.StateClosed:
		return statusClosed{}
	case model.StateInProgress:
		return statusInProgress{}
	}
	if len(openBlockers) > 0 {
		return statusBlocked{blocker: openBlockers[0]}
	}
	return statusReady{}
}

// buildEpicContext resolves an epic and its children into an EpicContext.
// focusedChildID is the child the caller is "at" ("" for none, e.g. an
// epic-level call). Each child's detail is fetched once and feeds both its
// status classification and the cross-epic edge collection, so there is one
// resolved source per child rather than a separate fetch per concern.
func buildEpicContext(ctx context.Context, st *store.Store, epicID, focusedChildID string) (EpicContext, error) {
	detail, err := st.GetIssueDetail(ctx, epicID)
	if err != nil {
		return EpicContext{}, err
	}
	internal := epicMemberIDs(detail.Issue.ID, detail.Children)
	children := make([]epicChild, 0, len(detail.Children))
	var cross crossEpicEdges
	for _, child := range detail.Children {
		childDetail, err := st.GetIssueDetail(ctx, child.ID)
		if err != nil {
			return EpicContext{}, err
		}
		// [LAW:one-source-of-truth] The row's issue, its status, its blockers,
		// and its cross-epic edges all derive from this one resolved detail —
		// never the epic-snapshot child, which could disagree if the child's
		// state changed between the epic fetch and this one.
		children = append(children, epicChild{
			Issue:  childDetail.Issue,
			Status: classifyChildStatus(childDetail.Issue, openBlockers(childDetail)),
		})
		cross.collect(childDetail, internal)
	}
	cross.sortByEndpoints()
	return EpicContext{Epic: detail.Issue, Children: children, Focused: focusedChildID, CrossEpic: cross}, nil
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

// openBlockers returns the ids of a resolved issue's still-open direct
// blockers, sorted by id so the blocker named in a blocked marker is
// deterministic. Same-epic blockers are kept — an inline blocked-by marker
// names whichever open blocker comes first, sibling or not — so nothing is
// excluded.
func openBlockers(detail model.IssueDetail) []string {
	var ids []string
	for _, dep := range openExcluding(detail.DependsOn, nil) {
		ids = append(ids, dep.ID)
	}
	sort.Strings(ids)
	return ids
}

// collect appends the boundary-crossing blocks edges incident to one internal
// child. A closed child carries no live plan context, so it contributes
// nothing; same-epic counterparts are excluded because their ordering is
// already conveyed by rank in the children list, and closed counterparts are
// dropped by openExcluding.
func (x *crossEpicEdges) collect(child model.IssueDetail, internal map[string]struct{}) {
	if child.Issue.State() == model.StateClosed {
		return
	}
	id := child.Issue.ID
	for _, blocker := range openExcluding(child.DependsOn, internal) {
		x.BlockedExternally = append(x.BlockedExternally, crossEpicEdge{Blocked: id, Blocker: blocker.ID})
	}
	for _, dependent := range openExcluding(child.Blocks, internal) {
		x.BlocksExternally = append(x.BlocksExternally, crossEpicEdge{Blocked: dependent.ID, Blocker: id})
	}
}

// openExcluding keeps the issues that are still open and whose ids are not in
// excluded. A nil excluded set drops nothing by membership, leaving the plain
// open filter; passing the epic's child ids drops same-epic counterparts so
// only boundary-crossing ones remain.
func openExcluding(others []model.Issue, excluded map[string]struct{}) []model.Issue {
	var out []model.Issue
	for _, other := range others {
		if _, skip := excluded[other.ID]; skip {
			continue
		}
		if other.State() != model.StateClosed {
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
	return fmt.Sprintf("%s%-*s %s  %s%s\n", prefix, statusMarkerWidth, child.Status.marker(), child.Issue.ID, child.Issue.Title, suffix)
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
