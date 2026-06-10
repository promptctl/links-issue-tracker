package cli

import (
	"fmt"
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
