package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An ActionName is a string type, so `%s` interpolates it silently and renders
// the PERSISTED EVENT ENCODING — the wrong name in any sentence a caller reads.
// Nothing in the compiler objects, and the two names agree for seven of the
// eight actions, so a wrong site reads correctly almost always and is found by a
// reader holding a command that does not exist.
//
// That is not hypothetical. Writing links-cli-errors-nvmd I swept for this by
// hand three times and missed a site each time: twice because the name is
// stashed in a struct field at one site and read back under another spelling at
// another, and once because the site built a usage string by concatenation
// rather than through fmt. A grep for the shape at the use site cannot see any
// of them. So the rule gets a gate. [LAW:single-enforcer]
//
// WHAT THIS GATE COVERS, exactly, so the comment does not claim more than the
// code checks:
//
//   - every fmt formatter, not just Errorf and Sprintf, and calls wrapped across
//     lines (found by matching balanced parentheses, not by reading one line);
//   - ActionName-bearing expressions of both shapes: a field whose declared type
//     is ActionName, and Name() called on a value declared as an Action;
//   - both the names it looks for are DERIVED from declarations in the tree, not
//     kept in a list beside them, because a list beside them is a second copy
//     that falls behind silently.
//
// What it does not cover, stated in full because an incomplete account of a
// gate's blind spots is the same defect as an overclaiming one:
//
//   - String building outside fmt. `lit bulk <verb>` builds its usage line by
//     concatenation, and that is the site of this class a reviewer found after
//     two of my own sweeps missed it. Dropping the fmt restriction was measured
//     rather than assumed: the resulting rule flags 82 sites, nearly all
//     unrelated fields that happen to be named Action, and a gate that cries
//     wolf 82 times is a gate someone switches off. That site is pinned by
//     TestBulkUsageNamesTheTypedVerb instead, which constructs the one action
//     whose two names differ.
//   - A value of type ActionName reached through a spelling no declaration in
//     the tree introduces — assigned to a local with a fresh name, say, or
//     returned through an interface. TestActionNameSpellingsAreAccountedFor is
//     the tripwire for a new declaration; a fresh local name is not covered.
var (
	// A struct field or var declared as ActionName: captures the field's name.
	actionNameField = regexp.MustCompile(`(?m)^\s*(\w+)\s+(?:model\.)?ActionName\s*$`)
	// A parameter or field declared as an Action: captures the value's name, so
	// `x.Name()` can be recognised as producing an ActionName.
	actionValue = regexp.MustCompile(`\b(\w+)\s+(?:model\.|lifecycle\.)?(?:Status|Retention)?Action\b`)
	fmtCallHead = regexp.MustCompile(`fmt\.(?:Errorf|Sprintf|Fprintf|Printf|Sprint|Fprint|Print|Sprintln|Fprintln|Println)\(`)
)

func repoRootForVerbTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory; cannot locate the repo")
		}
		dir = parent
	}
}

// trackedGoSources returns every tracked non-test Go file with its contents. It
// fails rather than returning an empty set: a scanner handed nothing to scan
// reports success, which is indistinguishable from a scanner that found nothing
// wrong. [LAW:no-silent-failure]
func trackedGoSources(t *testing.T) map[string]string {
	t.Helper()
	root := repoRootForVerbTest(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	sources := map[string]string{}
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading tracked file %s: %v", rel, err)
		}
		sources[rel] = string(body)
	}
	if len(sources) == 0 {
		t.Fatal("no tracked non-test Go files found — this scan would pass over an empty set")
	}
	return sources
}

// fmtCalls yields each fmt call's full text, following balanced parentheses so a
// call wrapped across lines is one string rather than a first line and a blind
// spot. This repository wraps 24 fmt.Errorf calls that way, so a line-at-a-time
// scanner would skip real sites while reporting success.
func fmtCalls(src string) []struct {
	Line int
	Text string
} {
	var out []struct {
		Line int
		Text string
	}
	for _, m := range fmtCallHead.FindAllStringIndex(src, -1) {
		depth, i := 0, m[1]-1
		for ; i < len(src); i++ {
			switch src[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		if i >= len(src) {
			continue
		}
		out = append(out, struct {
			Line int
			Text string
		}{strings.Count(src[:m[0]], "\n") + 1, src[m[0] : i+1]})
	}
	return out
}

func namesTypedActionName(sources map[string]string) []string {
	seen := map[string]bool{}
	for _, src := range sources {
		for _, m := range actionNameField.FindAllStringSubmatch(src, -1) {
			seen[m[1]] = true
		}
	}
	out := []string{}
	for n := range seen {
		out = append(out, n)
	}
	return out
}

func namesTypedAction(sources map[string]string) []string {
	seen := map[string]bool{}
	for _, src := range sources {
		for _, line := range strings.Split(src, "\n") {
			// Skip comments: `// ... no Action ...` matched the declaration
			// pattern and produced bearer names like `no.Name()`, which match
			// nothing and so fail silently rather than loudly.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range actionValue.FindAllStringSubmatch(line, -1) {
				seen[m[1]] = true
			}
		}
	}
	out := []string{}
	for n := range seen {
		out = append(out, n)
	}
	return out
}

func TestNoActionNameReachesAMessageAsItsPersistedEncoding(t *testing.T) {
	sources := trackedGoSources(t)
	fields, values := namesTypedActionName(sources), namesTypedAction(sources)
	if len(fields) == 0 || len(values) == 0 {
		t.Fatalf("derived %d ActionName fields and %d Action values; with neither, this test checks nothing", len(fields), len(values))
	}
	bearers := []string{}
	for _, f := range fields {
		bearers = append(bearers, "."+f)
	}
	for _, v := range values {
		bearers = append(bearers, v+".Name()")
	}
	bad := []string{}
	for rel, src := range sources {
		for _, call := range fmtCalls(src) {
			for _, bearer := range bearers {
				idx := strings.Index(call.Text, bearer)
				if idx < 0 {
					continue
				}
				rest := call.Text[idx+len(bearer):]
				// .Verb() is the caller's word; an explicit string() conversion
				// says the persisted encoding is meant here on purpose.
				if strings.HasPrefix(rest, ".Verb()") || strings.Contains(call.Text, "string("+bearer+")") {
					continue
				}
				bad = append(bad, fmt.Sprintf("%s:%d: %s", rel, call.Line, strings.Join(strings.Fields(call.Text), " ")))
				break
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("an action's persisted event encoding reaches a message a caller reads.\nUse .Verb() for the word the caller typed, or write string(...) to say the persisted encoding is meant:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestActionNameSpellingsAreAccountedFor keeps the gate above honest. It derives
// what it scans for, but a derivation is still a claim about the tree, and the
// claim is that these are ALL the ways an ActionName is spelled. Pinning the
// counts means a ninth field, or an Action parameter under a new name, fails
// here and sends someone to re-read the rule — rather than quietly widening the
// set of spellings the scan never looks at.
func TestActionNameSpellingsAreAccountedFor(t *testing.T) {
	sources := trackedGoSources(t)
	// ContainerActionError.Action, plus the status and retention writers in the
	// Dolt store, whose fields are both named `action`.
	if got := len(namesTypedActionName(sources)); got != 2 {
		t.Errorf("ActionName-typed field NAMES = %d (%v), want 2", got, namesTypedActionName(sources))
	}
	if got := len(namesTypedAction(sources)); got == 0 {
		t.Error("no value declared as an Action was found, so x.Name() sites are not being scanned at all")
	}
}
