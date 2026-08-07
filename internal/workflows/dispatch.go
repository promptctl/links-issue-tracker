package workflows

import (
	"fmt"
	"io"
	"strings"
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
// [LAW:effects-at-boundaries] w and workspaceRoot are the only effects; the
// matching itself stays the pure logic in match.go.
//
// Recording the occasion for observability/tracing is a later ticket in the
// same epic (.5); it extends this function's body directly in place rather
// than registering a handler with it. [LAW:single-enforcer] Wiring every
// command through this one call means that ticket touches one function, not
// every call site again.
//
// Load/parse problems surface as Set.Warnings, not here: a malformed
// definition must never break the command that triggered this Dispatch, so
// this function never inspects or prints them, and none are surfaced to the
// user yet — a known, tracked gap, not a silent design claim.
// [LAW:no-silent-failure] Dispatch deliberately doesn't fill that gap itself:
// w is the calling command's own agent-facing stdout, so printing an
// unrelated workflow-authoring warning into it would surface a config
// diagnostic on every single invocation, not just the ones that touch
// workflows. Displaying Set.Warnings with real detail (source layer, path,
// which definition) is promptctl-orchestration-ffqz.4's job ("the see-it
// surface"); a partial version here would leave two places deciding how
// workflow warnings render. [LAW:single-enforcer]
func Dispatch(w io.Writer, workspaceRoot string, o Occasion) error {
	set := Load(workspaceRoot)
	for _, def := range set.Matching(o) {
		if _, err := fmt.Fprintln(w, interpolate(def.Body, o.IssueID)); err != nil {
			return err
		}
	}
	return nil
}

// interpolate replaces the <id> placeholder in an injected body with the
// occasion's issue id. It is the one substitution workflow bodies get today —
// the guidance mechanism this replaces also supported a <token> placeholder,
// but its only caller always passed the empty string, so there was nothing
// live to carry forward.
func interpolate(body, issueID string) string {
	return strings.ReplaceAll(body, "<id>", issueID)
}
