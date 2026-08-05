package workflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// writeWorkflow writes one definition file under root, creating parents.
func writeWorkflow(t *testing.T, root string, rel string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// isolate points the global config layer at an empty temp dir and returns a
// fresh workspace root, so tests never see the developer's real config.
func isolate(t *testing.T) (workspaceRoot string, globalWorkflows string) {
	t.Helper()
	xdgRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgRoot)
	return t.TempDir(), filepath.Join(xdgRoot, "links-issue-tracker", "workflows")
}

func TestLoadRecursiveDiscoveryUnderArbitraryHierarchy(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	// The hierarchy is arbitrary: nesting depth and folder names carry no
	// meaning, only the frontmatter does.
	writeWorkflow(t, project, "top.md", "---\nevents: [show_backlog]\n---\ntop")
	writeWorkflow(t, project, "a/deep/nested/leaf.md", "---\nlabels: [x]\n---\nleaf")
	writeWorkflow(t, project, "with space/file name.md", "---\nevents: [show_ticket]\n---\nspaced")
	writeWorkflow(t, project, "ignored.txt", "not a workflow")

	set := Load(workspaceRoot)
	if len(set.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", set.Warnings)
	}
	var ids []string
	for _, def := range set.Definitions {
		ids = append(ids, def.ID)
	}
	want := []string{"a/deep/nested/leaf", "top", "with_space/file_name"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("Definitions ids = %v, want %v (sorted)", ids, want)
	}
}

func TestLoadProjectOverridesGlobalByID(t *testing.T) {
	workspaceRoot, globalWorkflows := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	// Same explicit id at both layers, authored at different paths: the
	// nearer (project) layer wins.
	writeWorkflow(t, project, "local/mine.md", "---\nid: shared\nevents: [show_ticket]\n---\nproject wins")
	writeWorkflow(t, globalWorkflows, "elsewhere/theirs.md", "---\nid: shared\nevents: [show_ticket]\n---\nglobal loses")
	writeWorkflow(t, globalWorkflows, "only-global.md", "---\nevents: [show_backlog]\n---\nglobal only")

	set := Load(workspaceRoot)
	if len(set.Definitions) != 2 {
		t.Fatalf("Definitions = %+v, want exactly 2", set.Definitions)
	}
	byID := map[string]Definition{}
	for _, def := range set.Definitions {
		byID[def.ID] = def
	}
	shared, ok := byID["shared"]
	if !ok {
		t.Fatalf("Definitions = %+v, missing id shared", set.Definitions)
	}
	if shared.Source != templates.SourceProject || shared.Body != "project wins" {
		t.Fatalf("shared = %+v, want the project layer's definition", shared)
	}
	if globalOnly, ok := byID["only-global"]; !ok || globalOnly.Source != templates.SourceGlobal {
		t.Fatalf("Definitions = %+v, want only-global from the global layer", set.Definitions)
	}
}

func TestLoadSameRelativePathOverridesWithoutExplicitID(t *testing.T) {
	workspaceRoot, globalWorkflows := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	// Default IDs derive from the relative path, so the same path at two
	// layers collides on id — which is exactly the override mechanism.
	writeWorkflow(t, project, "review/design.md", "---\nevents: [show_ticket]\n---\nproject")
	writeWorkflow(t, globalWorkflows, "review/design.md", "---\nevents: [show_ticket]\n---\nglobal")

	set := Load(workspaceRoot)
	if len(set.Definitions) != 1 {
		t.Fatalf("Definitions = %+v, want the project one only", set.Definitions)
	}
	if def := set.Definitions[0]; def.Source != templates.SourceProject || def.Body != "project" {
		t.Fatalf("definition = %+v, want project layer", def)
	}
}

func TestLoadDuplicateIDWithinLayerWarnsFirstWins(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	// Walk order is lexical: "a/dup.md" is seen before "b/dup.md".
	writeWorkflow(t, project, "a/dup.md", "---\nid: dup\nevents: [show_ticket]\n---\nfirst")
	writeWorkflow(t, project, "b/dup.md", "---\nid: dup\nevents: [show_ticket]\n---\nsecond")

	set := Load(workspaceRoot)
	if len(set.Definitions) != 1 || set.Definitions[0].Body != "first" {
		t.Fatalf("Definitions = %+v, want only the first file", set.Definitions)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0].Message, `duplicate id "dup"`) {
		t.Fatalf("Warnings = %v, want one duplicate-id warning", set.Warnings)
	}
	if set.Warnings[0].Path != "b/dup.md" {
		t.Fatalf("warning path = %q, want the ignored file", set.Warnings[0].Path)
	}
}

func TestLoadAbsentLayersYieldEmptySet(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	set := Load(workspaceRoot)
	if len(set.Definitions) != 0 || len(set.Warnings) != 0 {
		t.Fatalf("Load() = %+v, want empty set with no warnings", set)
	}
	// An absent workspace root means the project layer contributes nothing.
	set = Load("")
	if len(set.Definitions) != 0 || len(set.Warnings) != 0 {
		t.Fatalf("Load(\"\") = %+v, want empty set with no warnings", set)
	}
}

func TestLoadMalformedFileWarnsAndSkipsWithoutBreakingOthers(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "good.md", "---\nevents: [show_backlog]\n---\ngood")
	writeWorkflow(t, project, "broken.md", "---\nlabels: 17\n---\nbad")

	set := Load(workspaceRoot)
	if len(set.Definitions) != 1 || set.Definitions[0].ID != "good" {
		t.Fatalf("Definitions = %+v, want only the good file", set.Definitions)
	}
	if len(set.Warnings) != 1 || set.Warnings[0].Path != "broken.md" {
		t.Fatalf("Warnings = %v, want one warning for broken.md", set.Warnings)
	}
}

func TestLoadInertFileLoadsWithWarningAndNeverFires(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "readme.md", "no frontmatter, just prose")

	set := Load(workspaceRoot)
	if len(set.Definitions) != 1 || !set.Definitions[0].Inert() {
		t.Fatalf("Definitions = %+v, want the inert definition loaded", set.Definitions)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0].Message, "inert") {
		t.Fatalf("Warnings = %v, want one inert warning", set.Warnings)
	}
	for _, event := range Catalog() {
		if got := set.Matching(Occasion{Event: event}); len(got) != 0 {
			t.Fatalf("Matching(%s) = %+v, want inert definition to fire nowhere", event, got)
		}
	}
}
