package workflows

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// frontmatter is the YAML shape of a definition file's header: the keys this
// lit reads, and every other key in Unread. [LAW:one-source-of-truth] the
// fields are the one list of keys lit reads; Unread is whatever they do not
// declare, so no second list can drift from them.
type frontmatter struct {
	ID     string         `yaml:"id"`
	Name   string         `yaml:"name"`
	When   []string       `yaml:"when"`
	Labels []string       `yaml:"labels"`
	Unread map[string]any `yaml:",inline"`
}

// ParseMoment reads one `when:` entry into the moment it names, the one reader
// of that spelling for the frontmatter, `lit workflows dry-run --when` and
// `lit workflows edit`: `enter <state>` or `exit <state>` is a Crossing of one
// of States, read through the lifecycle's own gates (so `in-progress` is
// in_progress), and any other text is an event of the Catalog. Case and runs
// of whitespace do not matter, like every frontmatter value, so the result is
// in the canonical form an Occasion carries. Text naming no moment this lit
// fires — blank, an unknown event, an unknown state — is an error saying why,
// which each caller reports as its own context needs.
// [LAW:parse-dont-validate] [LAW:single-enforcer]
func ParseMoment(text string) (Moment, error) {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return nil, errors.New("a blank entry names no moment")
	}
	if side := Side(words[0]); len(words) > 1 && (side == Enter || side == Exit) {
		state, err := parseState(strings.Join(words[1:], " "))
		if err != nil {
			return nil, err
		}
		return Crossing{Side: side, State: state}, nil
	}
	event := Event(strings.Join(words, " "))
	if !slices.Contains(Catalog(), event) {
		names := make([]string, len(Catalog()))
		for i, known := range Catalog() {
			names[i] = string(known)
		}
		return nil, fmt.Errorf("unknown event %q: the events are %s, and a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])", event, strings.Join(names, ", "))
	}
	return event, nil
}

// parseState reads a crossing's state: a status (model.ParseState) or a phase
// inside in_progress (model.ParsePhase).
func parseState(text string) (string, error) {
	if status, err := model.ParseState(text); err == nil {
		return string(status), nil
	}
	if phase, err := model.ParsePhase(text); err == nil {
		return string(phase), nil
	}
	return "", fmt.Errorf("unknown state %q: a ticket's states are %s", text, strings.Join(States(), ", "))
}

const frontmatterDelimiter = "---"

// isDelimiter is the one definition of a frontmatter delimiter line: "---"
// alone, tolerating trailing horizontal whitespace and a CR — the convention
// every major frontmatter implementation (Jekyll, Hugo) follows.
// [LAW:one-source-of-truth] Opening and closing checks share it so they
// cannot drift apart.
func isDelimiter(line string) bool {
	return strings.TrimRight(line, " \t\r") == frontmatterDelimiter
}

// splitFrontmatter divides a definition file into its YAML header and
// markdown body. A file that never opens a frontmatter block is all body
// (found=false); a block opened but never closed is an error the caller
// reports as malformed.
func splitFrontmatter(content string) (header string, body string, found bool, err error) {
	firstLine, rest, _ := strings.Cut(content, "\n")
	if !isDelimiter(firstLine) {
		return "", content, false, nil
	}
	var headerLines []string
	remaining := rest
	for {
		line, tail, more := strings.Cut(remaining, "\n")
		if isDelimiter(line) {
			return strings.Join(headerLines, "\n"), tail, true, nil
		}
		if !more {
			return "", "", false, errors.New("unterminated frontmatter: missing closing ---")
		}
		headerLines = append(headerLines, line)
		remaining = tail
	}
}

// parseDefinition turns one file's raw content into a Definition and the
// warnings reading it produced. Every file yields a definition, and one lit
// cannot fire on its keys is Never, holding its id all the same: dropping it
// would fire a farther layer's definition of that id in its place. What fires
// for an id is what its nearest author wrote, or nothing.
// [LAW:no-silent-failure] every reason a file fires less than it says is a
// warning, and a broken file never breaks a lit invocation.
//
// A key lit does not read makes the file Never, because lit cannot tell
// whether it was meant to narrow when the definition fires (a `where:` written
// for a newer lit, an `events:` from an older one beside `labels:`). A key
// whose every entry is unreadable makes it Never too: the key can never hold,
// and dropping it would widen the definition to the other key alone.
func parseDefinition(content, path string, source templates.Source) (Definition, []Warning) {
	warn := func(message string) Warning {
		return Warning{Source: source, Path: path, Message: message}
	}

	meta, body, err := readFrontmatter(content)
	def := Definition{
		ID:         precedence.First(strings.TrimSpace(meta.ID), defaultID(path)),
		Name:       strings.TrimSpace(meta.Name),
		Activation: Never{},
		Body:       strings.TrimSpace(body),
		Source:     source,
		Path:       path,
	}
	if err != nil {
		return def, []Warning{warn(err.Error())}
	}

	when, warnings := parseWhen(meta.When, warn)
	labels, labelWarnings := canonicalLabels(meta.Labels, warn)
	warnings = append(warnings, labelWarnings...)
	switch {
	case len(meta.Unread) > 0:
		warnings = append(warnings, warn(unreadKeysMessage(slices.Sorted(maps.Keys(meta.Unread)))))
	case len(meta.When) == 0 && len(meta.Labels) == 0:
		warnings = append(warnings, warn("no activation keys (when/labels): the definition never fires"))
	case len(meta.When) > 0 && len(when) == 0, len(meta.Labels) > 0 && len(labels) == 0:
		// The key can never hold; its entries' warnings already say why.
	default:
		def.Activation = Keys{When: when, Labels: labels}
	}
	return def, warnings
}

// readFrontmatter is a file's header decoded as far as it decodes, its body,
// and the error that stopped the read, if one did. A yaml type error still
// decodes the rest, so a file whose `when:` is a scalar keeps the id it
// declares.
func readFrontmatter(content string) (frontmatter, string, error) {
	var meta frontmatter
	header, body, _, err := splitFrontmatter(content)
	if err != nil {
		return meta, "", err
	}
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return meta, body, fmt.Errorf("invalid frontmatter: %w", err)
	}
	return meta, body, nil
}

// unreadKeysMessage names the keys lit did not read and the spelling it does,
// so an author holding a file written for another lit sees what to write.
func unreadKeysMessage(keys []string) string {
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = strconv.Quote(key)
	}
	return fmt.Sprintf("this lit does not read %s, so the definition never fires; it reads when: for the moments a definition fires at (when: [enter closed], when: [show_ticket]) and labels: for the tickets it fires on", strings.Join(quoted, ", "))
}

// parseWhen reads a `when:` list through ParseMoment: the moments it names,
// each once, and a warning for each entry that names none. An unreadable
// entry never fires; the rest of the list still does, since entries are
// alternatives — and so is a repeat, which names nothing the list lacks.
func parseWhen(entries []string, warn func(string) Warning) ([]Moment, []Warning) {
	var out []Moment
	var warnings []Warning
	for _, entry := range entries {
		moment, err := ParseMoment(entry)
		if err != nil {
			warnings = append(warnings, warn(fmt.Sprintf("when: entry %q never fires: %v", entry, err)))
			continue
		}
		if !slices.Contains(out, moment) {
			out = append(out, moment)
		}
	}
	return out, warnings
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
// construction. An entry the canonical form rejects (a blank one, or a comma
// label that can never exist on a ticket) is dropped with a warning, never
// silently. [LAW:parse-dont-validate]
func canonicalLabels(values []string, warn func(string) Warning) ([]string, []Warning) {
	var out []string
	var warnings []Warning
	for _, value := range values {
		normalized, err := model.NormalizeLabel(value)
		if err != nil {
			warnings = append(warnings, warn(fmt.Sprintf("label %q can never match: %v", value, err)))
			continue
		}
		out = append(out, normalized)
	}
	return out, warnings
}
