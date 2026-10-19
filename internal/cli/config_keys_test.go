package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A key a config layer sets that is no setting is named, with its file, once
// per command on stderr and once on the command's event, by bare lit and by
// app-mode and workspace-mode commands alike, even though the commands here
// read the config more than once. The command still runs.
func TestUnknownConfigKeysWarnOncePerCommand(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	globalFile, projectFile := writeConfigLayers(t, repo,
		"[init]\ninstall_hooks = false\n",
		"[snapshot]\nretention_budgt = 2\n")
	wantLines := []string{
		"warning: " + globalFile + ` sets "init.install_hooks", which this version of lit does not read, so it has no effect`,
		"warning: " + projectFile + ` sets "snapshot.retention_budgt", which this version of lit does not read, so it has no effect`,
	}
	wantFact := []any{
		map[string]any{"key": "init.install_hooks", "file": globalFile},
		map[string]any{"key": "snapshot.retention_budgt", "file": projectFile},
	}

	for _, args := range [][]string{{}, {"backlog"}, {"quickstart"}} {
		command := strings.Join(append([]string{"lit"}, args...), " ")
		t.Run(command, func(t *testing.T) {
			var out string
			var err error
			stderr := captureStderr(t, func() { out, err = runCLIInDirAllowError(t, repo, args...) })
			if err != nil {
				t.Fatalf("%s error = %v, want it to run\noutput:\n%s", command, err, out)
			}
			if strings.Contains(out, "warning:") {
				t.Errorf("%s stdout carries a warning:\n%s", command, out)
			}
			if got := warningLines(stderr); !reflect.DeepEqual(got, wantLines) {
				t.Errorf("stderr warnings = %q, want %q", got, wantLines)
			}
			ev := lastEvent(t)
			if got := ev.Facts["config.unknown_keys"]; !reflect.DeepEqual(got, wantFact) {
				t.Errorf("config.unknown_keys = %#v, want %#v", got, wantFact)
			}
			if got, present := ev.Facts["config.unknown_keys_error"]; !present || got != nil {
				t.Errorf("config.unknown_keys_error = %#v (present %v), want null", got, present)
			}
		})
	}
}

// Config layers that set only settings print no warning, and the event
// records that the keys were checked and none was unknown: [], not absent.
func TestKnownConfigKeysWarnNothing(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	writeConfigLayers(t, repo, "required_fields = [\"title\"]\n", "[snapshot]\nretention_budget = 2\n")

	stderr := captureStderr(t, func() { runCLIInDir(t, repo, "backlog") })
	if got := warningLines(stderr); len(got) != 0 {
		t.Errorf("stderr warnings = %q, want none", got)
	}
	if got, present := lastEvent(t).Facts["config.unknown_keys"]; !present || !reflect.DeepEqual(got, []any{}) {
		t.Errorf("config.unknown_keys = %#v (present %v), want []", got, present)
	}
}

// A layer that does not parse is named in a warning and on the event, and
// the keys of the layer that does parse are still reported.
func TestUnparseableConfigLayerWarnsAndHidesNoOtherKey(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	globalFile, projectFile := writeConfigLayers(t, repo, "[init]\ninstall_hooks = false\n", "[sync\n")

	stderr := captureStderr(t, func() { _, _ = runCLIInDirAllowError(t, repo, "backlog") })
	got := warningLines(stderr)
	if len(got) != 2 ||
		!strings.HasPrefix(got[0], "warning: config keys were not checked: parse config "+projectFile+":") ||
		got[1] != "warning: "+globalFile+` sets "init.install_hooks", which this version of lit does not read, so it has no effect` {
		t.Errorf("stderr warnings = %q, want the project file's parse failure, then the global file's unknown key", got)
	}
	ev := lastEvent(t)
	wantFact := []any{map[string]any{"key": "init.install_hooks", "file": globalFile}}
	if got := ev.Facts["config.unknown_keys"]; !reflect.DeepEqual(got, wantFact) {
		t.Errorf("config.unknown_keys = %#v, want %#v", got, wantFact)
	}
	if got, _ := ev.Facts["config.unknown_keys_error"].(string); !strings.Contains(got, projectFile) {
		t.Errorf("config.unknown_keys_error = %#v, want the parse failure naming %s", ev.Facts["config.unknown_keys_error"], projectFile)
	}
}

// writeConfigLayers writes the global layer under a fresh XDG_CONFIG_HOME and
// the project layer into repo, returning each file's path as lit names it.
func writeConfigLayers(t *testing.T, repo, global, project string) (globalFile, projectFile string) {
	t.Helper()
	globalFile = filepath.Join(t.TempDir(), "links-issue-tracker", "config.toml")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(filepath.Dir(globalFile)))
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	projectFile = filepath.Join(root, ".lit", "config.toml")
	for path, body := range map[string]string{globalFile: global, projectFile: project} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return globalFile, projectFile
}

// warningLines is every line of stderr that is a warning, in order.
func warningLines(stderr string) []string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "warning: ") {
			lines = append(lines, line)
		}
	}
	return lines
}
