package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

// renderedGrouping parses `lit --help` (the rendered usage) into the group each
// command appears under, plus where each group header falls, so assertions read
// the observable help output rather than the internal commandGroups slice.
// [LAW:behavior-not-structure] the contract is what `--help` shows an agent, not
// how the registry is stored.
func renderedGrouping(t *testing.T) (groupOf map[string]string, headerLine map[string]int) {
	t.Helper()
	root := newRootCommand(context.Background(), io.Discard, io.Discard)
	groupOf = map[string]string{}
	headerLine = map[string]int{}
	current := ""
	for i, line := range strings.Split(root.UsageString(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "  ") { // "  <name>   <summary>" — a command row
			if fields := strings.Fields(line); len(fields) > 0 && current != "" {
				groupOf[fields[0]] = current
			}
			continue
		}
		// A non-indented line is a section header. The non-group scaffolding
		// sections (Usage/Flags/Additional Commands/the "Use ..." footer) are not
		// command groups, so they clear the current group rather than becoming one.
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Usage:", trimmed == "Flags:",
			strings.HasPrefix(trimmed, "Additional"), strings.HasPrefix(trimmed, "Use \""):
			current = ""
		default:
			current = trimmed
			headerLine[current] = i
		}
	}
	return groupOf, headerLine
}

// The state-transition surface splits across two help groups so the high-traffic
// status lifecycle stands out: the core verbs stay in Agent Operations, the rare
// retention verbs move to their own Issue Retention group rendered below it. This
// pins that split — the acceptance criterion for regrouping the transition verbs —
// against the rendered `lit --help`.
func TestTransitionVerbGrouping(t *testing.T) {
	groupOf, headerLine := renderedGrouping(t)

	for _, name := range []string{"start", "done", "close", "open"} {
		if got := groupOf[name]; got != "Agent Operations" {
			t.Fatalf("core lifecycle verb %q renders under %q, want Agent Operations", name, got)
		}
	}
	for _, name := range []string{"archive", "unarchive", "delete", "restore"} {
		if got := groupOf[name]; got != "Issue Retention" {
			t.Fatalf("retention verb %q renders under %q, want Issue Retention (demoted out of Agent Operations)", name, got)
		}
	}

	// Issue Retention must render below Agent Operations, or the demotion that
	// keeps the core verbs prominent would not hold in the output an agent reads.
	ops, okOps := headerLine["Agent Operations"]
	ret, okRet := headerLine["Issue Retention"]
	if !okOps || !okRet {
		t.Fatalf("help is missing a group header: Agent Operations present=%v, Issue Retention present=%v", okOps, okRet)
	}
	if ret <= ops {
		t.Fatalf("Issue Retention header at line %d, want below Agent Operations at line %d", ret, ops)
	}
}
