package workflows

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// frontmatter is the YAML shape of a definition file's header. Unknown keys
// are deliberately tolerated: a file authored for a newer lit must still load
// here with the keys this binary understands.
type frontmatter struct {
	ID     string            `yaml:"id"`
	Name   string            `yaml:"name"`
	Labels []string          `yaml:"labels"`
	States []StateActivation `yaml:"states"`
	Events []string          `yaml:"events"`
}

// UnmarshalYAML accepts a state activation in either authored shape: a bare
// scalar state name (meaning enter), or a mapping with `name` and an optional
// `when: enter|exit`.
func (s *StateActivation) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		s.State = canonicalState(node.Value)
		s.When = WhenEnter
		return nil
	case yaml.MappingNode:
		var aux struct {
			Name string `yaml:"name"`
			When string `yaml:"when"`
		}
		if err := node.Decode(&aux); err != nil {
			return err
		}
		when, err := parseWhen(aux.When)
		if err != nil {
			return err
		}
		name := canonicalState(aux.Name)
		// A mapping entry carries authored intent; refusing a nameless one here
		// keeps that intent from vanishing in a silent drop downstream.
		// [LAW:parse-dont-validate]
		if name == "" {
			return fmt.Errorf("state entry mapping requires a non-empty name")
		}
		s.State = name
		s.When = when
		return nil
	default:
		return fmt.Errorf("state entry must be a state name or a {name, when} mapping")
	}
}

// parseWhen is the one place a raw `when:` value becomes a When: absent means
// enter, anything but enter/exit is a parse error the caller reports as a
// malformed file. [LAW:parse-dont-validate]
func parseWhen(raw string) (When, error) {
	switch When(strings.TrimSpace(raw)) {
	case "", WhenEnter:
		return WhenEnter, nil
	case WhenExit:
		return WhenExit, nil
	default:
		return "", fmt.Errorf("when must be %q or %q, got %q", WhenEnter, WhenExit, raw)
	}
}

const frontmatterDelimiter = "---"

// splitFrontmatter divides a definition file into its YAML header and
// markdown body. A file that never opens a frontmatter block is all body
// (found=false); a block opened but never closed is an error the caller
// reports as malformed.
func splitFrontmatter(content string) (header string, body string, found bool, err error) {
	firstLine, rest, _ := strings.Cut(content, "\n")
	if strings.TrimRight(firstLine, "\r") != frontmatterDelimiter {
		return "", content, false, nil
	}
	var headerLines []string
	remaining := rest
	for {
		line, tail, more := strings.Cut(remaining, "\n")
		if strings.TrimRight(line, "\r") == frontmatterDelimiter {
			return strings.Join(headerLines, "\n"), tail, true, nil
		}
		if !more {
			return "", "", false, errors.New("unterminated frontmatter: missing closing ---")
		}
		headerLines = append(headerLines, line)
		remaining = tail
	}
}

// parseDefinition turns one file's raw content into a Definition. Malformed
// frontmatter yields ok=false plus a warning — a broken workflow file must
// never break a lit invocation. A well-formed file always yields a
// definition; missing activation keys and unknown events are loaded as
// authored, each with a warning so the mistake is observable rather than
// silently inert. [LAW:no-silent-failure]
func parseDefinition(content, path string, source templates.Source) (Definition, []Warning, bool) {
	warn := func(message string) Warning {
		return Warning{Source: source, Path: path, Message: message}
	}

	header, body, _, err := splitFrontmatter(content)
	if err != nil {
		return Definition{}, []Warning{warn(err.Error())}, false
	}
	var meta frontmatter
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return Definition{}, []Warning{warn("invalid frontmatter: " + err.Error())}, false
	}

	var warnings []Warning
	labels, labelWarns := canonicalLabels(meta.Labels, warn)
	warnings = append(warnings, labelWarns...)

	def := Definition{
		ID:     precedence.First(strings.TrimSpace(meta.ID), defaultID(path)),
		Name:   strings.TrimSpace(meta.Name),
		Labels: labels,
		States: compactStates(meta.States),
		Events: canonicalEvents(meta.Events),
		Body:   strings.TrimSpace(body),
		Source: source,
		Path:   path,
	}
	if def.Inert() {
		warnings = append(warnings, warn("no activation keys (labels/states/events): definition is inert and will never fire"))
	}
	for _, event := range def.Events {
		if !event.Known() {
			warnings = append(warnings, warn(fmt.Sprintf("unknown event %q: not in this lit's catalog, will never fire here", event)))
		}
	}
	return def, warnings, true
}

// defaultID derives a definition's ID from its layer-relative path: the ".md"
// suffix dropped and spaces replaced by underscores. Same relative path at
// two layers therefore defaults to the same ID, which is what makes layer
// override work without authoring an explicit id.
func defaultID(path string) string {
	return strings.ReplaceAll(strings.TrimSuffix(path, ".md"), " ", "_")
}

// canonicalLabels stamps authored label entries into the domain's canonical
// form (model.NormalizeLabel — the same form the store persists), so the
// matcher's exact comparison against stored labels is case-insensitive by
// construction. Blank entries are dropped as authoring noise; an entry the
// canonical form rejects (a comma label can never exist on a ticket) is
// dropped with a warning, never silently. [LAW:parse-dont-validate]
func canonicalLabels(values []string, warn func(string) Warning) ([]string, []Warning) {
	var out []string
	var warnings []Warning
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		normalized, err := model.NormalizeLabel(value)
		if err != nil {
			warnings = append(warnings, warn(fmt.Sprintf("label %q can never match: %v", value, err)))
			continue
		}
		out = append(out, normalized)
	}
	return out, warnings
}

// canonicalState stamps an authored state name into canonical form: trimmed
// and lowercased, matching how the lifecycle spells the states an Occasion
// carries. Lowercase-at-the-parse-boundary is the one convention shared by
// all three activation dimensions; a future custom-stage feature inherits it.
func canonicalState(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// canonicalEvents stamps authored event entries into the catalog's canonical
// form: trimmed and lowercased, empties dropped. Catalog names are lowercase
// snake_case by contract (the catalog test enforces it), so lowercasing here
// can never collide two distinct events — it only resolves case variants to
// the form the catalog speaks, symmetric with label canonicalization.
// [LAW:parse-dont-validate]
func canonicalEvents(values []string) []Event {
	var out []Event
	for _, value := range values {
		if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
			out = append(out, Event(trimmed))
		}
	}
	return out
}

// compactStates drops activations whose state name trimmed to nothing. Only
// bare empty scalars can reach here — a nameless mapping is already rejected
// at unmarshal — so what drops is authoring noise with no intent attached,
// exactly like an empty label or event entry.
func compactStates(activations []StateActivation) []StateActivation {
	var out []StateActivation
	for _, activation := range activations {
		if activation.State != "" {
			out = append(out, activation)
		}
	}
	return out
}
