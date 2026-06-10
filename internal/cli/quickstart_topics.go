package cli

import (
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

const quickstartUsage = "usage: lit quickstart [ready|new|update|done|doctor] [--refresh] [--eject[=LIST]] [--force]"

// quickstartTopics maps `lit quickstart <topic>` tokens to their guidance
// templates, in router display order.
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
