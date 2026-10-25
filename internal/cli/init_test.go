package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func writeProjectTemplateOverride(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, ".lit", "templates", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(content)
}

// init's event says whether this run built the database: the first init in a
// repository did, a second one found it there.
func TestInitEventSaysWhetherItBuiltTheDatabase(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	runGit(t, dir, "init")
	for _, want := range []struct {
		created   bool
		bootstrap string
	}{{true, "built"}, {false, "none"}} {
		runCLIInDir(t, dir, "init", "--skip-hooks")
		facts := writtenFacts(t, lastEvent(t))
		if facts["init.db_created"] != want.created || facts["bootstrap"] != want.bootstrap {
			t.Fatalf("init event init.db_created, bootstrap = %v, %v; want %v, %v",
				facts["init.db_created"], facts["bootstrap"], want.created, want.bootstrap)
		}
	}
}

// A command whose open finds the database gone builds it again and says so on
// its event, a write command and a sync command alike; a command that finds it
// there says it built nothing.
func TestAnOpenThatBuildsTheDatabaseSaysSoOnItsEvent(t *testing.T) {
	isolateEvents(t)
	dir := t.TempDir()
	runGit(t, dir, "init")
	runCLIInDir(t, dir, "init", "--skip-hooks")
	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	for _, step := range []struct {
		removeDatabase bool
		args           []string
		want           string
	}{
		{true, []string{"new", "--title", "rebuilt", "--topic", "demo", "--type", "epic"}, "built"},
		{false, []string{"sync", "status"}, "none"},
		{true, []string{"sync", "status"}, "built"},
	} {
		if step.removeDatabase {
			if err := os.RemoveAll(ws.DatabasePath); err != nil {
				t.Fatalf("remove the database: %v", err)
			}
		}
		runCLIInDir(t, dir, step.args...)
		if got := writtenFacts(t, lastEvent(t))["bootstrap"]; got != step.want {
			t.Fatalf("lit %s event bootstrap = %v, want %v", strings.Join(step.args, " "), got, step.want)
		}
	}
}
