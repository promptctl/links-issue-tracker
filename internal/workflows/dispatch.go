package workflows

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Dispatch is the one seam every curated command routes its occasions
// through: synchronous, in-process, called directly from the command path —
// no bus, no async queue, no subscriber list. [LAW:no-mode-explosion] There
// is exactly one Dispatch, not a registry other packages add themselves to.
//
// It loads the workspace's full definition Set, matches it against the
// occasion, and writes every match's body to w — the same agent-facing
// stream the calling command already writes to — in Set.Matching's stable ID
// order, each with <id> interpolated to the occasion's IssueID.
// [LAW:effects-at-boundaries] w, errOut, and ws are the only effects; the
// matching itself stays the pure logic in match.go. errOut is the diagnostic
// stream a trace-write failure goes to — a workflows package function never
// hardcodes os.Stderr itself; every CLI call site passes it explicitly, so
// the decision of where diagnostics land lives at the boundary, not inside
// this leaf/matching package, and a test can capture or discard it freely.
//
// When at least one definition fires, Dispatch also records a firing trace
// (trace.go) — which definitions fired and why (Definition.MatchReasons) —
// under the workspace's shared trace storage, so `lit workflows dry-run` and
// a real invocation explain "why did this fire" through the same mechanism.
// An occasion nothing matches leaves no trace: the directory stays
// proportional to guidance actually injected, not to every lit invocation.
// Recording is skipped outright when ws.StorageDir isn't an absolute path: a
// workspace.Info resolved the real way (workspace.Resolve, always rooted at
// git-common-dir) is never relative, so this only guards against a caller
// holding a partially-populated workspace.Info with no trustworthy place to
// write — never a state real usage can reach.
// [LAW:no-defensive-null-guards] the guard sits at Dispatch's own trust
// boundary (an arbitrary caller-supplied struct), not around an invariant
// this package could enforce on its own input type.
// A trace-write failure never fails Dispatch — the guidance was already
// written to w — it goes to stderr instead, the same best-effort shape
// recordMirrorTraceError uses for the sibling automation trace.
// [LAW:no-silent-failure] the failure is never swallowed, only kept off the
// agent-facing stream.
//
// Load/parse problems never break the command that triggered this Dispatch:
// each warning the load produced goes to errOut, one line apiece, and back to
// the caller, so a definition that stopped firing — a file written for
// another lit, say — is announced by the command it no longer fires on, not
// only by `lit workflows`. [LAW:no-silent-failure] errOut, not w: w is the
// command's own agent-facing stdout, where a config diagnostic would read as
// guidance. Warning.String is the one rendering, shared with `lit workflows`.
// [LAW:single-enforcer]
//
// It returns the definitions that fired and the load's warnings, so the
// caller can say on its own record what fired and what the load reported;
// they are returned with a write error too, since they fired whether or not
// their bodies reached w.
func Dispatch(env environ.Env, w io.Writer, errOut io.Writer, ws workspace.Info, o Occasion) ([]Definition, []Warning, error) {
	set := Load(env, ws.RootDir)
	warnings := set.WarningsByLayer()
	for _, warning := range warnings {
		fmt.Fprintf(errOut, "lit: workflow %s\n", warning)
	}
	matched := set.Matching(o)
	for _, def := range matched {
		if _, err := fmt.Fprintln(w, Interpolate(def.Body, o.IssueID)); err != nil {
			return matched, warnings, err
		}
	}
	if len(matched) > 0 && filepath.IsAbs(ws.StorageDir) {
		if _, err := recordFiring(ws, o, matched); err != nil {
			fmt.Fprintf(errOut, "lit: workflow firing trace could not be recorded (%v); guidance was still injected\n", err)
		}
	}
	return matched, warnings, nil
}

// Interpolate replaces the <id> placeholder in an injected body with the
// occasion's issue id, held to one row as %s holds it: the id may arrive by
// sync, and a break in it would forge lines in the guidance an agent reads.
// It is the one substitution workflow bodies get.
// Exported so `lit workflows dry-run` can preview the same interpolated body
// Dispatch would actually inject.
func Interpolate(body string, issueID model.ID) string {
	return strings.ReplaceAll(body, "<id>", fmt.Sprint(issueID))
}
