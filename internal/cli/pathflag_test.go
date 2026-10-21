package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A relative path a command is given names a file under the directory the
// invocation ran in. The harness hands Run that directory and never moves the
// process into it, so a path resolved against the process's own directory
// would name a file that is not there.
func TestRelativePathsResolveAgainstTheInvocationDirectory(t *testing.T) {
	isolateEvents(t)
	repo, id, _ := statuslineCheckout(t)
	sub := filepath.Join(repo, "notes")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if wd, err := os.Getwd(); err != nil || wd == sub {
		t.Fatalf("process cwd = %q, %v; the test needs it to differ from %q", wd, err, sub)
	}

	// A long-text file flag, which reads its own file.
	if err := os.WriteFile(filepath.Join(sub, "body.md"), []byte("relative body"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLIInDir(t, sub, "comment", "add", id, "--body-file", "body.md")
	if got, want := commentBodies(t, repo), []string{"relative body"}; !reflect.DeepEqual(got, want) {
		t.Errorf("comment bodies = %q, want %q", got, want)
	}

	// A pathFlag, which parseLeaf resolves before the leaf runs.
	if err := os.WriteFile(filepath.Join(sub, "export.json"), []byte(runCLIInDir(t, repo, "export")), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runCLIInDir(t, sub, "backup", "restore", "--path", "export.json", "--force")
	if want := "restored " + filepath.Join(sub, "export.json") + "\n"; !strings.HasPrefix(out, want) {
		t.Errorf("restore output = %q, want it to start %q", out, want)
	}
}

// A path flag is resolved only when it was given, and a given value follows
// one rule: whitespace around it is not part of it, and an empty or
// flag-shaped one is refused as usage rather than resolved to the working
// directory or to a file named "--help".
func TestResolvePathsResolvesOnlyAGivenPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		args  []string
		want  string
		usage bool
	}{
		{"absent", nil, "", false},
		{"relative", []string{"--path", " sub/file.json "}, filepath.Join(dir, "sub", "file.json"), false},
		{"absolute", []string{"--path", "/abs/file.json"}, "/abs/file.json", false},
		{"empty", []string{"--path="}, "", true},
		{"blank", []string{"--path", "  "}, "", true},
		{"flag-shaped", []string{"--path", "--help"}, "", true},
	} {
		fs := newCobraFlagSet("probe")
		flag := registerPath(fs, "path", "a path")
		if err := parseFlagSet(fs, tc.args, io.Discard); err != nil {
			t.Fatalf("%s: parse error = %v", tc.name, err)
		}
		err := resolvePaths(fs, WorkingDirOf(dir, nil))
		var usage UsageError
		if refused := errors.As(err, &usage); refused != tc.usage || (err != nil && !refused) {
			t.Errorf("%s: resolvePaths error = %v, want a UsageError: %t", tc.name, err, tc.usage)
		}
		if got := flag.path(); got != tc.want {
			t.Errorf("%s: path() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An unreadable working directory fails only a path that needs it.
func TestUnreadableWorkingDirFailsOnlyARelativePath(t *testing.T) {
	t.Parallel()
	gone := WorkingDirOf("", os.ErrNotExist)
	if got, err := gone.resolve("/abs/file.json"); err != nil || got != "/abs/file.json" {
		t.Errorf("resolve(absolute) = %q, %v; want it unchanged", got, err)
	}
	if _, err := gone.resolve("file.json"); err == nil {
		t.Error("resolve(relative) succeeded against an unreadable working directory")
	}
}

// A working directory is absolute or it is an error: an empty or relative one,
// or an invocation given none, would leave the process's directory to resolve
// against.
func TestWorkingDirIsAbsoluteOrAnError(t *testing.T) {
	t.Parallel()
	for name, dir := range map[string]WorkingDir{
		"zero":     {},
		"empty":    WorkingDirOf("", nil),
		"relative": WorkingDirOf("sub", nil),
	} {
		if got, err := dir.Path(); err == nil {
			t.Errorf("%s: Path() = %q, nil; want an error", name, got)
		}
		if got, err := dir.resolve("file.json"); err == nil {
			t.Errorf("%s: resolve(relative) = %q, nil; want an error", name, got)
		}
	}
	if got, err := WorkingDirOf("/abs", nil).Path(); err != nil || got != "/abs" {
		t.Errorf("absolute: Path() = %q, %v; want /abs", got, err)
	}
}
