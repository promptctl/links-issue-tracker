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
	pullForward := fs.Bool("pull-forward", false, "Promote blockers to match this issue's priority")
	pushBack := fs.Bool("push-back", false, "Demote this issue to match its worst blocker's priority")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit fix-priority <issue-id> --pull-forward | --push-back")
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: lit fix-priority <issue-id> --pull-forward | --push-back")
	}
	if *pullForward == *pushBack {
		return fmt.Errorf("specify exactly one of --pull-forward or --push-back")
	}

	issue, err := ap.Store.GetIssue(ctx, positional[0])
	if err != nil {
		return err
	}

	writeJSON := shouldWriteJSON(stdout, *jsonOut)
	if *pushBack {
		return fixPriorityPushBack(ctx, stdout, ap, issue, writeJSON)
	}
	return fixPriorityPullForward(ctx, stdout, ap, issue, writeJSON)
}

type priorityChange struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	OldPriority int    `json:"old_priority"`
	NewPriority int    `json:"new_priority"`
}

// fixPriorityPushBack demotes the issue to match its worst direct blocker's priority.
func fixPriorityPushBack(ctx context.Context, stdout io.Writer, ap *app.App, issue model.Issue, jsonOut bool) error {
	detail, err := ap.Store.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		return err
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
		return printValue(stdout, []priorityChange{}, jsonOut, printPriorityChangesAny)
	}
	if err := ap.Store.UpdatePriorities(ctx, []store.PriorityUpdate{{ID: issue.ID, NewPriority: worstPri}}); err != nil {
		return err
	}
	changes := []priorityChange{{
		ID:          issue.ID,
		Title:       issue.Title,
		OldPriority: issue.Priority,
		NewPriority: worstPri,
	}}
	return printValue(stdout, changes, jsonOut, printPriorityChangesAny)
}

// fixPriorityPullForward promotes all blockers in the dependency chain
// to at least the issue's priority.
func fixPriorityPullForward(ctx context.Context, stdout io.Writer, ap *app.App, issue model.Issue, jsonOut bool) error {
	targetPri := issue.Priority
	var changes []priorityChange
	// [LAW:dataflow-not-control-flow] BFS walks the full dependency graph;
	// nodes that already meet the priority threshold produce no updates.
	queue := []string{issue.ID}
	visited := map[string]bool{issue.ID: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		detail, err := ap.Store.GetIssueDetail(ctx, current)
		if err != nil {
			return err
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
