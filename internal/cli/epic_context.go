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
// plus its children in rank order, each pre-classified, plus the id of the
// focused child (empty when none). It is the seam between data resolution
// (buildEpicContext) and rendering (renderEpicContext); siblings extend this
// value rather than the renderer.
type EpicContext struct {
	Epic     model.Issue
	Children []epicChild
	Focused  string
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
// epic-level call).
func buildEpicContext(ctx context.Context, st *store.Store, epicID, focusedChildID string) (EpicContext, error) {
	detail, err := st.GetIssueDetail(ctx, epicID)
	if err != nil {
		return EpicContext{}, err
	}
	children := make([]epicChild, 0, len(detail.Children))
	for _, child := range detail.Children {
		blockers, err := openBlockerIDs(ctx, st, child.ID)
		if err != nil {
			return EpicContext{}, err
		}
		children = append(children, epicChild{
			Issue:  child,
			Status: classifyChildStatus(child, blockers),
		})
	}
	return EpicContext{Epic: detail.Issue, Children: children, Focused: focusedChildID}, nil
}

// openBlockerIDs returns the ids of an issue's still-open direct blockers,
// sorted by id so the blocker named in a blocked marker is deterministic.
func openBlockerIDs(ctx context.Context, st *store.Store, issueID string) ([]string, error) {
	detail, err := st.GetIssueDetail(ctx, issueID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, dep := range detail.DependsOn {
		if dep.State() != model.StateClosed {
			ids = append(ids, dep.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
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
	if len(ec.Children) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}
	for _, child := range ec.Children {
		b.WriteString(renderChildLine(child, child.Issue.ID == ec.Focused))
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
