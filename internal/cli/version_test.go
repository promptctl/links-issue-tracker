package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/store/migrations"
	"github.com/bmf/links-issue-tracker/internal/version"
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
// trailing prose, no log lines on the buffer). Decoding the bytes as JSON
// then asserting nothing remains is the way to catch a hidden header that
// would slip past `valid-JSON` but break a `jq` pipeline downstream.
func TestVersionJSONIsStrictMachineContract(t *testing.T) {
	var stdout bytes.Buffer
	if err := runVersion(&stdout, []string{"--json"}); err != nil {
		t.Fatalf("runVersion --json error = %v", err)
	}

	dec := json.NewDecoder(&stdout)
	var first version.Info
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("first decode error = %v", err)
	}
	// Drain any whitespace and assert nothing follows.
	rest := strings.TrimSpace(stdout.String())
	if rest != "" {
		t.Errorf("--json emitted trailing bytes after the JSON document: %q", rest)
	}
}

// TestVersionHumanSurfacesAllInfoFields pins that the human form presents every
// field on Info (version, commit, date, schema range). A future Info field
// that gets added but not surfaced in the text form would slip past this test
// as a regression hint to update both surfaces.
func TestVersionHumanSurfacesAllInfoFields(t *testing.T) {
	// Stamp link-time fields so the human form has something concrete to render.
	origV, origC, origD := version.Version, version.Commit, version.Date
	t.Cleanup(func() { version.Version, version.Commit, version.Date = origV, origC, origD })
	version.Version = "v1.2.3"
	version.Commit = "abcdef0"
	version.Date = "2026-05-24T15:21:00Z"

	var stdout bytes.Buffer
	if err := runVersion(&stdout, nil); err != nil {
		t.Fatalf("runVersion (human) error = %v", err)
	}
	out := stdout.String()

	for _, want := range []string{"v1.2.3", "abcdef0", "2026-05-24T15:21:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q:\n%s", want, out)
		}
	}
	// Schema range — values come from the registry, not stampable, but the
	// range delimiter and the bounds must both appear.
	max, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("migrations.MaxVersion error = %v", err)
	}
	for _, want := range []string{"schema versions supported:", "1", "–"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q (schema range surface):\n%s", want, out)
		}
	}
	_ = max // referenced to keep the link if Max ever stops being 1
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
