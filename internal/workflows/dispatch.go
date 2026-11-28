package workflows

import (
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Firing is what one occasion fired: the definitions, in Set.Matching's
// stable ID order, the guidance they inject, and why the firing trace was not
// recorded, nil when it was or when nothing fired.
type Firing struct {
	Fired    []Match
	Guidance string
	Untraced error
}

// Fire is the one seam every command fires an occasion through: synchronous,
// in-process, called directly from the command path — no bus, no async queue,
// no subscriber list. [LAW:no-mode-explosion] There is exactly one Fire, not a
// registry other packages add themselves to.
//
// It matches the occasion against set, which the command loaded once for its
// whole run (Load). The guidance is each fired body with <id> interpolated to
// the occasion's IssueID, one line after another, which the command prints
// where its own output puts it. [LAW:effects-at-boundaries] matching and the
// guidance are values; Fire's one effect is the firing trace.
//
// When at least one definition fires, Fire records a firing trace (trace.go)
// — which definitions fired and why (Match.Reasons) — under the
// workspace's shared trace storage, so `lit workflows dry-run` and a real
// invocation explain "why did this fire" through the same mechanism. An
// occasion nothing matches leaves no trace: the directory stays proportional
// to guidance actually fired, not to every lit invocation. A trace-write
// failure never fails the command — the definitions fired all the same — so
// it comes back as Untraced, for the command to report beside the firing it
// failed to trace. [LAW:no-silent-failure]
func Fire(set Set, ws workspace.Info, o Occasion) Firing {
	firing := Firing{Fired: set.Matching(o)}
	var guidance strings.Builder
	for _, match := range firing.Fired {
		guidance.WriteString(Interpolate(match.Definition.Body, o.IssueID) + "\n")
	}
	firing.Guidance = guidance.String()
	// [LAW:dataflow-not-control-flow] exception: the trace is skipped when
	// nothing fired, so the trace directory grows with guidance fired, not
	// with every lit invocation.
	if len(firing.Fired) > 0 {
		firing.Untraced = recordFiring(ws, o, firing.Fired)
	}
	return firing
}

// Interpolate replaces the <id> placeholder in an injected body with the
// occasion's issue id, held to one row as %s holds it: the id may arrive by
// sync, and a break in it would forge lines in the guidance an agent reads.
// It is the one substitution workflow bodies get.
// Exported so `lit workflows dry-run` can preview the same interpolated body
// Fire actually injects.
func Interpolate(body string, issueID model.ID) string {
	return strings.ReplaceAll(body, "<id>", fmt.Sprint(issueID))
}
