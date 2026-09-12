package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// The embedded defaults must survive the parse byte-for-byte: they are what
// every already-initialized repo carries inside its markers, so a parse that
// re-shaped them would rewrite the managed block in every repo on the next
// `lit init`.
func TestParseLeavesEmbeddedDefaultsByteIdentical(t *testing.T) {
	for name, pair := range map[string]markerPair{
		templates.AgentsSectionTemplateName: litAgentsMarkers,
		templates.PrePushHookTemplateName:   litHookMarkers,
	} {
		embedded, err := templates.EmbeddedDefault(name)
		if err != nil {
			t.Fatalf("EmbeddedDefault(%s) error = %v", name, err)
		}
		section, err := pair.parse(string(embedded))
		if err != nil {
			t.Fatalf("parse(%s embedded default) error = %v", name, err)
		}
		if section.text != string(embedded) {
			t.Fatalf("parse(%s embedded default) = %q, want it unchanged", name, section.text)
		}
	}
}

func TestParseWrapsMarkerlessContent(t *testing.T) {
	pair := litAgentsMarkers
	section, err := pair.parse("## House rules\n\nRun lit quickstart.\n")
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	want := pair.begin + "\n## House rules\n\nRun lit quickstart.\n" + pair.end + "\n"
	if section.text != want {
		t.Fatalf("parse() = %q, want %q", section.text, want)
	}
}

// A marker-less override with no trailing newline would otherwise put the END
// marker on the last content line, where the next parse could no longer find a
// whole block on its own line.
func TestParseWrapsMarkerlessContentMissingTrailingNewline(t *testing.T) {
	pair := litAgentsMarkers
	section, err := pair.parse("## House rules")
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	want := pair.begin + "\n## House rules\n" + pair.end + "\n"
	if section.text != want {
		t.Fatalf("parse() = %q, want %q", section.text, want)
	}
}

// Whitespace outside the marker span sits outside the region upsert replaces,
// so it would compound one blank line per run. The pass-through canonicalizes.
func TestParseCanonicalizesWhitespaceAroundMarkedBlock(t *testing.T) {
	pair := litAgentsMarkers
	block := pair.begin + "\nbody\n" + pair.end
	section, err := pair.parse("\n\n  " + block + "\n\n\n")
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	if section.text != block+"\n" {
		t.Fatalf("parse() = %q, want %q", section.text, block+"\n")
	}
}

// Every shape that is neither "no markers" nor "exactly one whole block" would
// leave part of itself outside the replaced span and re-append that part on
// every run. Each is refused before anything is written.
func TestParseRejectsShapesThatCannotConverge(t *testing.T) {
	pair := litAgentsMarkers
	for name, content := range map[string]string{
		"begin only":            pair.begin + "\nbody\n",
		"end only":              "body\n" + pair.end + "\n",
		"reversed":              pair.end + "\nbody\n" + pair.begin + "\n",
		"text before block":     "preamble\n" + pair.begin + "\nbody\n" + pair.end + "\n",
		"text after block":      pair.begin + "\nbody\n" + pair.end + "\ntrailer\n",
		"two blocks":            pair.begin + "\na\n" + pair.end + "\n" + pair.begin + "\nb\n" + pair.end + "\n",
		"stray extra begin":     pair.begin + "\n" + pair.begin + "\nbody\n" + pair.end + "\n",
		"nested marker in body": pair.begin + "\nbody " + pair.end + " more\n" + pair.end + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			section, err := pair.parse(content)
			if err == nil {
				t.Fatalf("parse(%q) = %q, want a refusal", content, section.text)
			}
			// A malformed override is a deterministic refusal the user fixes in
			// their template file, not a transient failure worth retrying, and
			// not one the command's own arguments can satisfy.
			var shape templateShapeError
			if !errors.As(err, &shape) {
				t.Fatalf("parse(%q) error = %v, want a templateShapeError", content, err)
			}
			if got := commandErrorReason(err); got != "template_shape_refused" {
				t.Fatalf("parse(%q) reason = %q, want template_shape_refused", content, got)
			}
			if rem := commandErrorRemediation(commandErrorReason(err)); !strings.Contains(rem, "template override") {
				t.Fatalf("parse(%q) remediation = %q, want it to point at the template file", content, rem)
			}
			if !strings.Contains(err.Error(), pair.begin) {
				t.Fatalf("parse(%q) error = %v, want it to name the markers", content, err)
			}
		})
	}
}
