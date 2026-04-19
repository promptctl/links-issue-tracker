package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEjectTemplatesWritesAllByDefault(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	results, err := ejectTemplates("all", false)
	if err != nil {
		t.Fatalf("ejectTemplates() error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for _, r := range results {
		if !r.Changed {
			t.Fatalf("%s: Changed = false, want true", r.Name)
		}
		info, statErr := os.Stat(r.Path)
		if statErr != nil {
			t.Fatalf("%s: stat %s error = %v", r.Name, r.Path, statErr)
		}
		if info.Size() == 0 {
			t.Fatalf("%s: file at %s is empty", r.Name, r.Path)
		}
	}
}

func TestEjectTemplatesAcceptsShortName(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	results, err := ejectTemplates("quickstart", false)
	if err != nil {
		t.Fatalf("ejectTemplates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "quickstart.md" {
		t.Fatalf("name = %q, want quickstart.md", results[0].Name)
	}
	// Sibling template must NOT have been written.
	siblingPath := filepath.Join(xdgRoot, "links-issue-tracker", "templates", "agents-section.md")
	if _, statErr := os.Stat(siblingPath); !os.IsNotExist(statErr) {
		t.Fatalf("eject=quickstart wrote sibling at %s (err=%v)", siblingPath, statErr)
	}
}

func TestEjectTemplatesIsAtomicOnConflict(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	// Pre-create one conflicting override.
	dir := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	quickstartPath := filepath.Join(dir, "quickstart.md")
	userContent := "my customized quickstart\n"
	if err := os.WriteFile(quickstartPath, []byte(userContent), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	results, err := ejectTemplates("all", false)
	if err != nil {
		t.Fatalf("ejectTemplates() returned error during plan phase: %v", err)
	}
	// No sibling may have been written because the plan phase detected a conflict.
	for _, sibling := range []string{"agents-section.md", "pre-push-hook.sh"} {
		p := filepath.Join(dir, sibling)
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Fatalf("atomicity violated: %s was written despite conflict (err=%v)", p, statErr)
		}
	}
	// Pre-existing override must be untouched.
	got, readErr := os.ReadFile(quickstartPath)
	if readErr != nil {
		t.Fatalf("ReadFile(quickstart) error = %v", readErr)
	}
	if string(got) != userContent {
		t.Fatalf("user override got clobbered: got %q, want %q", string(got), userContent)
	}
	// Every result should surface the outcome (either "exists" or abort-skip).
	sawConflict := false
	for _, r := range results {
		if r.Skipped == "exists" {
			sawConflict = true
		}
		if r.Changed {
			t.Fatalf("%s: Changed=true in atomic abort; want false", r.Name)
		}
	}
	if !sawConflict {
		t.Fatal("no result reported the conflict")
	}
}

func TestEjectTemplatesForceOverwrites(t *testing.T) {
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)

	dir := filepath.Join(xdgRoot, "links-issue-tracker", "templates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	quickstartPath := filepath.Join(dir, "quickstart.md")
	if err := os.WriteFile(quickstartPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := ejectTemplates("quickstart", true); err != nil {
		t.Fatalf("ejectTemplates(force) error = %v", err)
	}
	content, err := os.ReadFile(quickstartPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) == "stale\n" {
		t.Fatal("force eject did not overwrite existing file")
	}
}

func TestEjectTemplatesRejectsUnknownAlias(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, err := ejectTemplates("bogus", false)
	if err == nil {
		t.Fatal("ejectTemplates(bogus) expected error, got nil")
	}
}
