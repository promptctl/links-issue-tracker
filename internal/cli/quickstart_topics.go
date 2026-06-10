package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// quickstartTopics maps `lit quickstart <topic>` tokens to their guidance
// templates, in router display order.
// [LAW:one-source-of-truth] This table is the sole declaration of topic membership and order;
// the usage string and the refresh template set derive from it.
// [LAW:one-type-per-behavior] Every topic is the same operation — load a guidance template, print it — varying only by value.
var quickstartTopics = []struct {
	Token    string
	Template string
}{
	{"ready", templates.QuickstartReadyTemplateName},
	{"new", templates.QuickstartNewTemplateName},
	{"update", templates.QuickstartUpdateTemplateName},
	{"done", templates.QuickstartDoneTemplateName},
	{"doctor", templates.QuickstartDoctorTemplateName},
}

func quickstartTopicTemplate(token string) (string, bool) {
	for _, topic := range quickstartTopics {
		if topic.Token == token {
			return topic.Template, true
		}
	}
	return "", false
}

func quickstartTopicTokens() []string {
	tokens := make([]string, 0, len(quickstartTopics))
	for _, topic := range quickstartTopics {
		tokens = append(tokens, topic.Token)
	}
	return tokens
}

// quickstartGuidanceTemplateNames returns the router template followed by the
// topic guidance templates, in router display order.
func quickstartGuidanceTemplateNames() []string {
	names := make([]string, 0, len(quickstartTopics)+1)
	names = append(names, templates.QuickstartTemplateName)
	for _, topic := range quickstartTopics {
		names = append(names, topic.Template)
	}
	return names
}

var quickstartUsage = fmt.Sprintf("usage: lit quickstart [%s] [--refresh] [--eject[=LIST]] [--force]", strings.Join(quickstartTopicTokens(), "|"))

// quickstartBreadcrumb is the one-line pointer mutation commands append to
// their text success output, so discovery of topic guidance happens at the
// moment of need rather than only at session start.
// [LAW:one-source-of-truth] The token must name a row of quickstartTopics;
// anything else is a wiring bug that fails loudly before any output is written.
func quickstartBreadcrumb(token string) string {
	if _, ok := quickstartTopicTemplate(token); !ok {
		panic(fmt.Sprintf("quickstart breadcrumb wired to unknown topic %q (valid: %s)", token, strings.Join(quickstartTopicTokens(), ", ")))
	}
	return "deeper guidance: lit quickstart " + token
}

// withQuickstartBreadcrumb wraps a text printer so the command's success
// output ends with the topic's breadcrumb line.
// [LAW:single-enforcer] JSON safety is structural, not guarded: printValue
// routes JSON mode to writeJSON before any text printer runs, so the
// breadcrumb cannot reach the machine contract on any stream.
func withQuickstartBreadcrumb(token string, textFn func(io.Writer, any) error) func(io.Writer, any) error {
	crumb := quickstartBreadcrumb(token)
	return func(w io.Writer, v any) error {
		if err := textFn(w, v); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, crumb)
		return err
	}
}
