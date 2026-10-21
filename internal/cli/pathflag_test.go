package cli

import (
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

// A path flag's value is resolved only when one was given: an absent or blank
// one names no path, rather than the working directory.
func TestResolvePathsLeavesAnAbsentFlagUnset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name, value, want string
	}{
		{"absent", "", ""},
		{"blank", "  ", ""},
		{"relative", " sub/file.json ", filepath.Join(dir, "sub", "file.json")},
		{"absolute", "/abs/file.json", "/abs/file.json"},
	} {
		fs := newCobraFlagSet("probe")
		flag := registerPath(fs, "path", "a path")
		*flag.raw = tc.value
		if err := resolvePaths(fs, WorkingDirOf(dir, nil)); err != nil {
			t.Fatalf("%s: resolvePaths error = %v", tc.name, err)
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
