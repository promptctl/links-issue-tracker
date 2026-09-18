package cli

import (
	"embed"
	"fmt"
	"slices"
	"strings"
)

// Command help text lives in the binary, never in a file the reader is told to
// go open. `lit` runs in consumer repositories; a help string naming
// `docs/cli-reference.md` resolves nowhere the tool is actually used, so the
// pointer dead-ends for every reader except a contributor standing in this
// source tree. [FRAMING:representation] a map of a territory the reader is not
// standing in is not a weaker map, it is a map of somewhere else.
//
// The text is embedded rather than written as Go constants so it stays plain
// prose — diffable, backtick-free, and editable without escaping — and it is
// deliberately NOT part of internal/templates: those are user-overridable
// guidance, while this describes what the binary actually does, and an
// override that drifted from the code would be help that lies about its own
// flags. [LAW:one-source-of-truth]
//
//go:embed helptext/*.txt
var helpTextFS embed.FS

// helpText returns the embedded long-form description for a command. The name
// is a file under helptext/; a missing one is a wiring bug, not a runtime
// condition, so it panics at the declaration that names it rather than
// shipping a command whose help is silently blank. [LAW:no-silent-failure]
func helpText(name string) string {
	body, err := helpTextFS.ReadFile("helptext/" + name + ".txt")
	if err != nil {
		panic(fmt.Sprintf("help text %q is not embedded: %v", name, err))
	}
	return strings.TrimRight(string(body), "\n")
}

// helpPage composes the one help layout every command renders: the long-form
// description, then the synopsis line that introduces the command's surface —
// a leaf's "Usage of <cmd>:" above its flag table, or a family's usage line.
// A command with no description renders the synopsis alone, exactly as it did
// before there was anywhere to put one.
//
// [LAW:single-enforcer] The two help-answering paths — parseFlagSet's leaf
// render and Run's HelpRequestedError render — compose their page here, so
// neither can grow its own spacing or ordering.
// [LAW:dataflow-not-control-flow] A description-less command is an empty
// section filtered out of the same join, so the page is one expression over
// values rather than two layouts selected by whether a description exists.
func helpPage(detail string, synopsis string) string {
	sections := []string{strings.TrimSpace(detail), strings.TrimSpace(synopsis)}
	return strings.Join(slices.DeleteFunc(sections, func(s string) bool { return s == "" }), "\n\n") + "\n"
}
