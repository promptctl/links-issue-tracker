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
//
// The two keys stay yaml nodes until readKey reads them: a node is zero only
// for a key left out, so a key written empty (`labels: []`, a bare `when:`)
// reads apart from one never written.
type frontmatter struct {
	ID     string         `yaml:"id"`
	Name   string         `yaml:"name"`
	When   yaml.Node      `yaml:"when"`
	Labels yaml.Node      `yaml:"labels"`
	Unread map[string]any `yaml:",inline"`
}

// ParseMoment reads one `when:` entry into the moment it names, the one reader
// of that spelling for the frontmatter, `lit workflows dry-run --when` and
// `lit workflows edit --when`: `enter <state>` or `exit <state>` is a Crossing
// of one of the lifecycle's state nodes, read through its own gate
// (model.ParseStateNode, so `in-progress` is in_progress), and any other text
// is an event of the Catalog. Case and runs
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
		state, err := model.ParseStateNode(strings.Join(words[1:], " "))
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
// cannot fire on its keys is never, holding its id all the same: dropping it
// would fire a farther layer's definition of that id in its place. What fires
// for an id is what its nearest author wrote, or nothing.
// [LAW:no-silent-failure] every reason a file fires less than it says is a
// warning, and a broken file never breaks a lit invocation.
//
// A key lit does not read makes the file never fire, because lit cannot tell
// whether it was meant to narrow when the definition fires (a `where:` written
// for a newer lit, an `events:` from an older one beside `labels:`). A key
// written with no entry lit reads makes it never fire too: the key can never
// hold, and dropping it would widen the definition to the other key alone.
func parseDefinition(content, path string, source templates.Source) (Definition, []Warning) {
	warn := func(message string) Warning {
		return Warning{Source: source, Path: path, Message: message}
	}

	meta, body, err := readFrontmatter(content)
	def := Definition{
		ID:     precedence.First(strings.TrimSpace(meta.ID), DefaultID(path)),
		Name:   strings.TrimSpace(meta.Name),
		Body:   strings.TrimSpace(body),
		Source: source,
		Path:   path,
	}
	if err != nil {
		return def, []Warning{warn(err.Error())}
	}

	when, whenHolds, warnings := readKey("when:", meta.When, ParseMoment, func(entry string, err error) string {
		return fmt.Sprintf("when: entry %q never fires: %v", entry, err)
	}, warn)
	labels, labelsHold, labelWarnings := readKey("labels:", meta.Labels, model.NormalizeLabel, func(entry string, err error) string {
		return fmt.Sprintf("label %q can never match: %v", entry, err)
	}, warn)
	warnings = append(warnings, labelWarnings...)
	switch {
	case len(meta.Unread) > 0:
		warnings = append(warnings, warn(unreadKeysMessage(slices.Sorted(maps.Keys(meta.Unread)))))
	case meta.When.IsZero() && meta.Labels.IsZero():
		warnings = append(warnings, warn("no activation keys (when/labels): the definition never fires"))
	case whenHolds && labelsHold:
		def.Activation = Activation{keys: &Keys{When: when, Labels: labels}}
	}
	return def, warnings
}

// readKey reads one activation key as its file wrote it. A key left out is
// the zero Key, which holds on anything. A written key is a list of entries
// read through read (Entries), each one it refuses a warning worded by
// refused; when none is left — `[]`, a bare key, every entry refused, or no
// list at all — the key can never hold, and holds is false.
func readKey[T comparable](name string, node yaml.Node, read func(string) (T, error), refused func(entry string, err error) string, warn func(string) Warning) (key Key[T], holds bool, warnings []Warning) {
	if node.IsZero() {
		return Key[T]{}, true, nil
	}
	var texts []string
	if err := node.Decode(&texts); err != nil {
		return Key[T]{}, false, []Warning{warn(fmt.Sprintf("invalid frontmatter: %s %v", name, err))}
	}
	entries, refusals := Entries(texts, read)
	for _, refusal := range refusals {
		warnings = append(warnings, warn(refused(refusal.Text, refusal.Err)))
	}
	if len(texts) == 0 {
		warnings = append(warnings, warn(name+" lists no entry, so the definition never fires"))
	}
	return Key[T]{entries: entries}, len(entries) > 0, warnings
}

// Refusal is one entry text Entries could not read, and why.
type Refusal struct {
	Text string
	Err  error
}

// Entries reads one key's entries, wherever they are written — a definition's
// `when:` or `labels:`, or the same key's flags on `lit workflows dry-run` and
// `edit` — each text through read, each value kept once in the order first
// written, and each text read refuses as a Refusal. A repeat names nothing
// the key lacks, so `labels: [x, X]` is one label and one reason.
// [LAW:single-enforcer] every surface reads a key's entries here, so they all
// keep and drop the same ones.
func Entries[T comparable](texts []string, read func(string) (T, error)) ([]T, []Refusal) {
	var entries []T
	var refusals []Refusal
	for _, text := range texts {
		entry, err := read(text)
		if err != nil {
			refusals = append(refusals, Refusal{Text: text, Err: err})
			continue
		}
		if !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	return entries, refusals
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

// DefaultID derives a definition's ID from its layer-relative path: the ".md"
// suffix dropped and spaces replaced by underscores. Same relative path at
// two layers therefore defaults to the same ID, which is what makes layer
// override work without authoring an explicit id.
func DefaultID(path string) string {
	return strings.ReplaceAll(strings.TrimSuffix(path, ".md"), " ", "_")
}
