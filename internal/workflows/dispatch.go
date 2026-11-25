package workflows

import (
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Fire is the one seam every command fires an occasion through: synchronous,
// in-process, called directly from the command path — no bus, no async queue,
// no subscriber list. [LAW:no-mode-explosion] There is exactly one Fire, not a
// registry other packages add themselves to.
//
// It matches the occasion against set, which the command loaded once for its
// whole run (Load), and returns the definitions that fired, in Set.Matching's
// stable ID order, and the guidance they inject: each body with <id>
// interpolated to the occasion's IssueID, one line after another. The command
// prints that guidance where its own output puts it.
// [LAW:effects-at-boundaries] matching and the guidance are values; Fire's one
// effect is the firing trace.
//
// When at least one definition fires, Fire records a firing trace (trace.go)
// — which definitions fired and why (Definition.MatchReasons) — under the
// workspace's shared trace storage, so `lit workflows dry-run` and a real
// invocation explain "why did this fire" through the same mechanism. An
// occasion nothing matches leaves no trace: the directory stays proportional
// to guidance actually fired, not to every lit invocation. A trace-write
// failure never fails the command — the definitions fired all the same — it
// goes to errOut instead, the same best-effort shape recordMirrorTraceError
// uses for the sibling automation trace. [LAW:no-silent-failure] the failure
// is never swallowed, only kept off the agent-facing stream.
func Fire(set Set, ws workspace.Info, errOut io.Writer, o Occasion) ([]Definition, string) {
	fired := set.Matching(o)
	var guidance strings.Builder
	for _, def := range fired {
		guidance.WriteString(Interpolate(def.Body, o.IssueID) + "\n")
	}
	if len(fired) > 0 {
		if _, err := recordFiring(ws, o, fired); err != nil {
			fmt.Fprintf(errOut, "lit: workflow firing trace could not be recorded (%v)\n", err)
		}
	}
	return fired, guidance.String()
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
