package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func runFixPriority(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("fix-priority")
	pullForward := fs.Bool("pull-forward", false, "Promote blockers to match issue priority")
	pushBack := fs.Bool("push-back", false, "Demote issue to match worst blocker priority")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit fix-priority [<issue-id>] [--pull-forward | --push-back]")
	}
	if *pullForward && *pushBack {
		return fmt.Errorf("specify at most one of --pull-forward or --push-back")
	}

	// [LAW:dataflow-not-control-flow] Strategy is computed once from flags;
	// dispatch branches on (hasIssue, strategy) without re-checking flags.
	usePushBack := *pushBack

	if len(positional) == 1 {
		issue, err := ap.Store.GetIssue(ctx, positional[0])
		if err != nil {
			return err
		}
		if usePushBack {
			return fixPriorityPushBack(ctx, stdout, ap, issue, *jsonOut)
		}
		return fixPriorityPullForward(ctx, stdout, ap, issue, *jsonOut)
	}

	if usePushBack {
		return fixAllPriorityPushBack(ctx, stdout, ap, *jsonOut)
	}
	return fixAllPriorityPullForward(ctx, stdout, ap, *jsonOut)
}

type priorityChange struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	OldPriority int    `json:"old_priority"`
	NewPriority int    `json:"new_priority"`
}

// computePushBackChanges returns the change needed to demote issue to match
// its worst direct blocker's priority, or nil if no inversion exists.
func computePushBackChanges(ctx context.Context, ap *app.App, issue model.Issue) ([]priorityChange, error) {
	detail, err := ap.Store.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		return nil, err
	}
	worstPri := issue.Priority
	for _, dep := range detail.DependsOn {
		if dep.Status == "closed" {
			continue
		}
		if dep.Priority > worstPri {
			worstPri = dep.Priority
		}
	}
	if worstPri == issue.Priority {
		return nil, nil
	}
	return []priorityChange{{
		ID:          issue.ID,
		Title:       issue.Title,
		OldPriority: issue.Priority,
		NewPriority: worstPri,
	}}, nil
}

// computePullForwardChanges walks the transitive dependency chain from issue
// and returns changes needed to promote all blockers to at least issue.Priority.
// visited is shared across calls to avoid redundant BFS walks.
func computePullForwardChanges(ctx context.Context, ap *app.App, issue model.Issue, visited map[string]bool) ([]priorityChange, error) {
	targetPri := issue.Priority
	var changes []priorityChange
	// [LAW:dataflow-not-control-flow] BFS walks the full dependency graph;
	// nodes that already meet the priority threshold produce no updates.
	queue := []string{issue.ID}
	visited[issue.ID] = true
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		detail, err := ap.Store.GetIssueDetail(ctx, current)
		if err != nil {
			return nil, err
		}
		for _, dep := range detail.DependsOn {
			if dep.Status == "closed" || visited[dep.ID] {
				continue
			}
			visited[dep.ID] = true
			queue = append(queue, dep.ID)
			if dep.Priority <= targetPri {
				continue
			}
			changes = append(changes, priorityChange{
				ID:          dep.ID,
				Title:       dep.Title,
				OldPriority: dep.Priority,
				NewPriority: targetPri,
			})
		}
	}
	return changes, nil
}

// fixPriorityPushBack demotes the issue to match its worst direct blocker's priority.
func fixPriorityPushBack(ctx context.Context, stdout io.Writer, ap *app.App, issue model.Issue, jsonOut bool) error {
	changes, err := computePushBackChanges(ctx, ap, issue)
	if err != nil {
		return err
	}
	return applyAndPrintPriorityChanges(ctx, stdout, ap, changes, jsonOut)
}

// fixPriorityPullForward promotes all blockers in the dependency chain
// to at least the issue's priority.
func fixPriorityPullForward(ctx context.Context, stdout io.Writer, ap *app.App, issue model.Issue, jsonOut bool) error {
	visited := map[string]bool{}
	changes, err := computePullForwardChanges(ctx, ap, issue, visited)
	if err != nil {
		return err
	}
	return applyAndPrintPriorityChanges(ctx, stdout, ap, changes, jsonOut)
}

// fixAllPriorityPullForward lists all open/in_progress issues, computes
// pull-forward changes for each, deduplicates, and applies in one batch.
func fixAllPriorityPullForward(ctx context.Context, stdout io.Writer, ap *app.App, jsonOut bool) error {
	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{
		Statuses: []string{"open", "in_progress"},
	})
	if err != nil {
		return err
	}
	var allChanges []priorityChange
	for _, issue := range issues {
		visited := map[string]bool{}
		changes, err := computePullForwardChanges(ctx, ap, issue, visited)
		if err != nil {
			return err
		}
		allChanges = append(allChanges, changes...)
	}
	// Dedup: when the same blocker appears from multiple dependents,
	// keep the best (lowest) NewPriority.
	allChanges = dedupPriorityChanges(allChanges)
	return applyAndPrintPriorityChanges(ctx, stdout, ap, allChanges, jsonOut)
}

// fixAllPriorityPushBack lists all open/in_progress issues, computes
// push-back changes for each, and applies in one batch.
func fixAllPriorityPushBack(ctx context.Context, stdout io.Writer, ap *app.App, jsonOut bool) error {
	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{
		Statuses: []string{"open", "in_progress"},
	})
	if err != nil {
		return err
	}
	var allChanges []priorityChange
	for _, issue := range issues {
		changes, err := computePushBackChanges(ctx, ap, issue)
		if err != nil {
			return err
		}
		allChanges = append(allChanges, changes...)
	}
	return applyAndPrintPriorityChanges(ctx, stdout, ap, allChanges, jsonOut)
}

// dedupPriorityChanges keeps only the best (lowest) NewPriority per issue ID.
func dedupPriorityChanges(changes []priorityChange) []priorityChange {
	best := map[string]priorityChange{}
	for _, c := range changes {
		existing, ok := best[c.ID]
		if !ok || c.NewPriority < existing.NewPriority {
			best[c.ID] = c
		}
	}
	deduped := make([]priorityChange, 0, len(best))
	// Preserve original ordering (first-seen order).
	seen := map[string]bool{}
	for _, c := range changes {
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		deduped = append(deduped, best[c.ID])
	}
	return deduped
}

// applyAndPrintPriorityChanges applies the given priority changes and prints output.
func applyAndPrintPriorityChanges(ctx context.Context, stdout io.Writer, ap *app.App, changes []priorityChange, jsonOut bool) error {
	if len(changes) == 0 {
		return printValue(stdout, []priorityChange{}, jsonOut, printPriorityChangesAny)
	}
	updates := make([]store.PriorityUpdate, len(changes))
	for i, c := range changes {
		updates[i] = store.PriorityUpdate{ID: c.ID, NewPriority: c.NewPriority}
	}
	if err := ap.Store.UpdatePriorities(ctx, updates); err != nil {
		return err
	}
	return printValue(stdout, changes, jsonOut, printPriorityChangesAny)
}

func printPriorityChangesAny(w io.Writer, v any) error {
	changes := v.([]priorityChange)
	if len(changes) == 0 {
		_, err := fmt.Fprintln(w, "No priority inversion found.")
		return err
	}
	for _, c := range changes {
		if _, err := fmt.Fprintf(w, "%s %q: P%d → P%d\n", c.ID, c.Title, c.OldPriority, c.NewPriority); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%d issue(s) updated.\n", len(changes))
	return err
}
