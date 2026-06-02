package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// TestVersionJSONMatchesGetOutput pins the JSON surface as the typed contract:
// the bytes `lit version --json` emits MUST round-trip via json.Unmarshal into
// a version.Info equal to version.Get(). Downstream tooling (`lit downgrade`,
// the refusal-message upgrade) reads this output; any drift between the
// command surface and the package surface breaks them.
//
// [LAW:one-source-of-truth] One source (version.Get); two presentations.
func TestVersionJSONMatchesGetOutput(t *testing.T) {
	var stdout bytes.Buffer
	if err := runVersion(&stdout, []string{"--json"}); err != nil {
		t.Fatalf("runVersion --json error = %v", err)
	}

	var got version.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\nbytes: %s", err, stdout.String())
	}
	want, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	if got != want {
		t.Errorf("--json decoded to %+v, want %+v", got, want)
	}
}

// TestVersionJSONIsStrictMachineContract pins [memory: json-mode-strict-machine-contract]:
// `--json` emits exactly one JSON document and nothing else (no banners, no
// trailing prose, no log lines on the buffer). A hidden header would slip
// past plain `json.Valid` (which only checks the first document) but break
// a `jq` pipeline downstream.
//
// Implementation: decode from a bytes.NewReader, then read EVERYTHING left
// — both bytes the decoder buffered ahead but didn't consume AND bytes the
// underlying reader still hasn't yielded. The combined leftover is the
// canonical "what came after the first JSON document"; only whitespace is
// allowed there (json.Encoder appends a trailing newline).
//
// (Note: we cannot inspect stdout.String() after Decode — json.Decoder
// buffers reads ahead, so the source bytes.Buffer still contains every byte
// written. Reading from dec.Buffered() ALONE is also insufficient when the
// decoder hasn't read past the first document. The combined dec.Buffered()
// + remainder-of-source via MultiReader is the robust shape.)
func TestVersionJSONIsStrictMachineContract(t *testing.T) {
	var stdout bytes.Buffer
	if err := runVersion(&stdout, []string{"--json"}); err != nil {
		t.Fatalf("runVersion --json error = %v", err)
	}

	raw := stdout.Bytes()
	reader := bytes.NewReader(raw)
	dec := json.NewDecoder(reader)
	var first version.Info
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("first decode error = %v", err)
	}

	// Drain decoder-buffered bytes + anything still unread on the reader.
	leftover, err := io.ReadAll(io.MultiReader(dec.Buffered(), reader))
	if err != nil {
		t.Fatalf("read leftover: %v", err)
	}
	tail := strings.TrimSpace(string(leftover))
	if tail != "" {
		t.Errorf("--json emitted trailing non-whitespace after the JSON document: %q", tail)
	}
}

// TestVersionHumanSurfacesAllInfoFields pins the currently-rendered Info
// fields in the human surface: Version, Commit, Date, and Schema.{Min,Max}.
// Scope is the present surface — this test does not provide automatic
// coverage for new Info fields. Adding a field to version.Info that should
// also appear in the human form requires updating this test explicitly.
// The pin is here so a refactor that drops one of the currently-rendered
// fields fails the build.
func TestVersionHumanSurfacesAllInfoFields(t *testing.T) {
	// Stamp link-time fields so the human form has something concrete to render.
	// Use values that can NOT appear anywhere except in their respective fields:
	// version is a sentinel that cannot collide with schema digits ("0.0.0" not
	// "v1.2.3"; the latter contains "1" which is the current Schema.Max).
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = "vSENTINEL-9.9.9"
	version.Commit = "abcdef0"
	version.Date = "2026-05-24T15:21:00Z"

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion (human) error = %v", err)
	}
	out := stdout.String()

	for _, want := range []string{"vSENTINEL-9.9.9", "abcdef0", "2026-05-24T15:21:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q:\n%s", want, out)
		}
	}

	// Schema range — assert the exact rendered substring, not loose substring
	// match. The format string is "schema versions supported: %d–%d\n", so the
	// expected line is fully determined by the registry bounds.
	min := migrations.Baseline
	max, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion error = %v", err)
	}
	wantLine := fmt.Sprintf("schema versions supported: %d–%d", min, max)
	if !strings.Contains(out, wantLine) {
		t.Errorf("human output missing schema-range line %q:\n%s", wantLine, out)
	}
}

// TestVersionHumanLabelsDevBuild pins the "no link-time version stamped"
// surface: the human form shows "dev" in place of an empty Version, and
// "unknown" in place of empty Commit/Date. Consumers of the human form rely
// on these labels (they're more legible than literal empty strings).
func TestVersionHumanLabelsDevBuild(t *testing.T) {
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = ""
	version.Commit = ""
	version.Date = ""

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion (dev) error = %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"dev", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("dev-build human output missing %q:\n%s", want, out)
		}
	}
}

// TestVersionRejectsPositionalArgs pins the command shape: `lit version` takes
// only `--json`; any positional arg is a usage error. Prevents silent misuse
// like `lit version v0.1.0` (which a user might think means "show v0.1.0's
// release manifest" — that operation belongs to a different command).
func TestVersionRejectsPositionalArgs(t *testing.T) {
	var stdout bytes.Buffer
	err := runVersion(&stdout, []string{"v0.1.0"})
	if err == nil {
		t.Fatal("runVersion with positional arg returned nil, want usage error")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("err = %v, want a usage error message", err)
	}
}
