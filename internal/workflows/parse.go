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

// ParseMoment reads one `when:` entry, the one reader of that spelling for
// the frontmatter, `lit workflows dry-run --when` and `lit workflows edit`:
// `enter <state>` or `exit <state>` is a Crossing, any other text an Event.
// Case and runs of whitespace do not matter, like every frontmatter value,
// so the result is in the canonical form an Occasion carries. Blank text is
// no moment (ok=false). [LAW:parse-dont-validate] [LAW:single-enforcer]
func ParseMoment(text string) (Moment, bool) {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return nil, false
	}
	if side := Side(words[0]); len(words) > 1 && (side == Enter || side == Exit) {
		return Crossing{Side: side, State: strings.Join(words[1:], " ")}, true
	}
	return Event(strings.Join(words, " ")), true
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

// parseDefinition turns one file's raw content into a Definition. Malformed
// frontmatter yields ok=false plus a warning — a broken workflow file must
// never break a lit invocation. A well-formed file always yields a
// definition; missing activation keys and unknown events are loaded as
// authored, each with a warning so the mistake is observable rather than
// silently inert. [LAW:no-silent-failure]
//
// A file carrying a key lit does not read yields a definition with no
// activation at all: it never fires, and it still holds its id, so a farther
// layer's definition of that id does not fire in its place. lit cannot tell
// whether the key was meant to narrow when the definition fires (a `where:`
// written for a newer lit, an `events:` from an older one beside `labels:`),
// so firing on the keys it does read could fire more than the author wrote,
// and dropping the file could fire what the author replaced. What fires for an
// id is what its nearest author wrote, or nothing. [LAW:no-silent-failure]
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

	def := Definition{
		ID:     precedence.First(strings.TrimSpace(meta.ID), defaultID(path)),
		Name:   strings.TrimSpace(meta.Name),
		Body:   strings.TrimSpace(body),
		Source: source,
		Path:   path,
	}
	if len(meta.Unread) > 0 {
		return def, []Warning{warn(unreadKeysMessage(slices.Sorted(maps.Keys(meta.Unread))))}, true
	}

	labels, warnings := canonicalLabels(meta.Labels, warn)
	def.When = parseWhen(meta.When)
	def.Labels = labels
	if def.Inert() {
		warnings = append(warnings, warn("no activation keys (when/labels): definition is inert and will never fire"))
	}
	for _, moment := range def.When {
		if !moment.Known() {
			warnings = append(warnings, warn(fmt.Sprintf("unknown event %q: not in this lit's catalog, will never fire here; a change of a ticket's state is written enter <state> or exit <state> (when: [enter closed])", moment)))
		}
	}
	return def, warnings, true
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

// parseWhen reads a `when:` list through ParseMoment, dropping blank entries
// as authoring noise, like an empty label.
func parseWhen(entries []string) []Moment {
	var out []Moment
	for _, entry := range entries {
		if moment, ok := ParseMoment(entry); ok {
			out = append(out, moment)
		}
	}
	return out
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
