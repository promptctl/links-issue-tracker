package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// shellHostileText is every character a one-argument body hands to the shell
// to parse — quotes, a backtick, a dollar sign, a blank line — so a body that
// lands intact proves the file and stdin paths carried it byte for byte.
const shellHostileText = "first line with `backticks`, $HOME and \"quotes\" and 'apostrophes'\n\nsecond paragraph"

// withStdin makes text the process's stdin for the rest of the test.
func withStdin(t *testing.T, text string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = file
	t.Cleanup(func() {
		os.Stdin = previous
		_ = file.Close()
	})
}

func writeTextFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "text.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// commentBodies is every comment body in the workspace at dir, read from the
// export rather than from `lit show`, which renders a body's newlines escaped.
func commentBodies(t *testing.T, dir string) []string {
	t.Helper()
	var export struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(runCLIInDir(t, dir, "export")), &export); err != nil {
		t.Fatal(err)
	}
	bodies := []string{}
	for _, c := range export.Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// sourceFacts is what the last invocation's event says about where each of its
// long-text fields came from.
func sourceFacts(t *testing.T) map[string]any {
	t.Helper()
	facts := map[string]any{}
	for key, value := range writtenFacts(t, lastEvent(t)) {
		if strings.HasSuffix(key, ".source") {
			facts[key] = value
		}
	}
	return facts
}

// A comment body arrives inline, from a file, or from stdin, lands exactly as
// written each way, and the event records which way it came.
func TestCommentAddTakesItsBodyInlineFromAFileOrFromStdin(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	file := writeTextFile(t, shellHostileText+" (file)\n")

	runCLIInDir(t, dir, "comment", "add", id, "--body", "inline body")
	if got, want := sourceFacts(t), map[string]any{"body.source": "inline"}; !reflect.DeepEqual(got, want) {
		t.Errorf("--body event facts = %v, want %v", got, want)
	}
	runCLIInDir(t, dir, "comment", "add", id, "--body-file", file)
	if got, want := sourceFacts(t), map[string]any{"body.source": "file"}; !reflect.DeepEqual(got, want) {
		t.Errorf("--body-file <path> event facts = %v, want %v", got, want)
	}
	withStdin(t, shellHostileText+" (stdin)\n")
	runCLIInDir(t, dir, "comment", "add", id, "--body-file", "-")
	if got, want := sourceFacts(t), map[string]any{"body.source": "stdin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("--body-file - event facts = %v, want %v", got, want)
	}

	want := []string{"inline body", shellHostileText + " (file)", shellHostileText + " (stdin)"}
	if got := commentBodies(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("stored comment bodies = %q, want %q", got, want)
	}
}

// Giving both --body and --body-file is refused as usage and writes nothing:
// neither flag silently outranks the other. An unreadable path is refused as
// usage too, naming the flag it came from.
func TestCommentAddRefusesTwoBodiesAndAnUnreadableFile(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	file := writeTextFile(t, "from the file")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--body", "inline", "--body-file", file}, "--body and --body-file both supply the same text; give one of them"},
		{[]string{"--body-file", filepath.Join(t.TempDir(), "missing.md")}, "read --body-file: open "},
	} {
		_, err := runCLIInDirAllowError(t, dir, append([]string{"comment", "add", id}, tc.args...)...)
		var usage UsageError
		if !errors.As(err, &usage) || !strings.HasPrefix(usage.Message, tc.want) {
			t.Errorf("comment add %v error = %v, want a UsageError starting %q", tc.args, err, tc.want)
		}
	}
	if got := commentBodies(t, dir); len(got) != 0 {
		t.Errorf("refused comment adds stored %q, want nothing", got)
	}
}

// new, followup and update read --description and --prompt from a file or
// stdin the same way comment add reads its body, and an update that supplies
// neither leaves both fields as they were.
func TestDescriptionAndPromptTakeAFileOrStdin(t *testing.T) {
	isolateEvents(t)
	dir, origin, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "long text")
	description := writeTextFile(t, shellHostileText+" (description)\n")
	prompt := writeTextFile(t, shellHostileText+" (prompt)\n")
	field := func(id, name string) string {
		return strings.TrimSuffix(runCLIInDir(t, dir, "show", id, "--field", name), "\n")
	}

	created := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "Long", "--topic", "demo", "--parent", epic, "--description-file", description, "--prompt-file", prompt))
	if got, want := sourceFacts(t), map[string]any{"description.source": "file", "prompt.source": "file"}; !reflect.DeepEqual(got, want) {
		t.Errorf("new event facts = %v, want %v", got, want)
	}
	if got := field(created, "description"); got != shellHostileText+" (description)" {
		t.Errorf("new --description-file stored %q", got)
	}
	if got := field(created, "prompt"); got != shellHostileText+" (prompt)" {
		t.Errorf("new --prompt-file stored %q", got)
	}

	withStdin(t, shellHostileText+" (followup)\n")
	followup := extractTicketID(t, runCLIInDir(t, dir, "followup", "--on", origin, "--title", "Next", "--description-file", "-"))
	if got, want := sourceFacts(t), map[string]any{"description.source": "stdin", "prompt.source": "absent"}; !reflect.DeepEqual(got, want) {
		t.Errorf("followup event facts = %v, want %v", got, want)
	}
	if got := field(followup, "description"); got != shellHostileText+" (followup)" {
		t.Errorf("followup --description-file - stored %q", got)
	}

	withStdin(t, "revised prompt")
	runCLIInDir(t, dir, "update", created, "--prompt-file", "-")
	if got, want := sourceFacts(t), map[string]any{"description.source": "absent", "prompt.source": "stdin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("update event facts = %v, want %v", got, want)
	}
	if got := field(created, "prompt"); got != "revised prompt" {
		t.Errorf("update --prompt-file - stored %q", got)
	}
	if got := field(created, "description"); got != shellHostileText+" (description)" {
		t.Errorf("update without a description flag changed the description to %q", got)
	}

	_, err := runCLIInDirAllowError(t, dir, "update", created, "--description", "inline", "--description-file", description)
	var usage UsageError
	if !errors.As(err, &usage) || usage.Message != "--description and --description-file both supply the same text; give one of them" {
		t.Errorf("update with both description flags error = %v, want the both-flags UsageError", err)
	}
}
