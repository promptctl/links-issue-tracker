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

// authored drops the embedded defaults every Load() carries, so a test of the
// project and global layers asserts on exactly the definitions it wrote.
func authored(set Set) []Definition {
	var out []Definition
	for _, def := range set.Definitions {
		if def.Source != templates.SourceEmbedded {
			out = append(out, def)
		}
	}
	return out
}

// findDefinition looks up one definition by id, so tests that authored their
// own definitions can assert on those without also enumerating the embedded
// defaults every Load() carries alongside them.
func findDefinition(set Set, id string) (Definition, bool) {
	for _, def := range set.Definitions {
		if def.ID == id {
			return def, true
		}
	}
	return Definition{}, false
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

	set := Load(testEnv(), workspaceRoot)
	if len(set.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", set.Warnings)
	}
	var ids []string
	for _, def := range authored(set) {
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

	set := Load(testEnv(), workspaceRoot)
	if len(authored(set)) != 2 {
		t.Fatalf("Definitions = %+v, want shared and only-global beside the embedded defaults", set.Definitions)
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

	set := Load(testEnv(), workspaceRoot)
	if len(authored(set)) != 1 {
		t.Fatalf("Definitions = %+v, want the project one beside the embedded defaults", set.Definitions)
	}
	def, ok := findDefinition(set, "review/design")
	if !ok || def.Source != templates.SourceProject || def.Body != "project" {
		t.Fatalf("Definitions = %+v, want review/design from the project layer", set.Definitions)
	}
}

func TestLoadDuplicateIDWithinLayerWarnsFirstWins(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	// Walk order is lexical: "a/dup.md" is seen before "b/dup.md".
	writeWorkflow(t, project, "a/dup.md", "---\nid: dup\nevents: [show_ticket]\n---\nfirst")
	writeWorkflow(t, project, "b/dup.md", "---\nid: dup\nevents: [show_ticket]\n---\nsecond")

	set := Load(testEnv(), workspaceRoot)
	if len(authored(set)) != 1 {
		t.Fatalf("Definitions = %+v, want the surviving file beside the embedded defaults", set.Definitions)
	}
	if dup, ok := findDefinition(set, "dup"); !ok || dup.Body != "first" {
		t.Fatalf("Definitions = %+v, want dup's body to be \"first\"", set.Definitions)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0].Message, `duplicate id "dup"`) {
		t.Fatalf("Warnings = %v, want one duplicate-id warning", set.Warnings)
	}
	if set.Warnings[0].Path != "b/dup.md" {
		t.Fatalf("warning path = %q, want the ignored file", set.Warnings[0].Path)
	}
}

// TestLoadAbsentLayersYieldsOnlyEmbeddedDefaults pins the floor: with no
// project or global layer on disk, Load still surfaces the definitions this
// binary ships — that's the whole point of the embedded layer existing.
func TestLoadAbsentLayersYieldsOnlyEmbeddedDefaults(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	set := Load(testEnv(), workspaceRoot)
	if len(set.Warnings) != 0 {
		t.Fatalf("Load() warnings = %v, want none", set.Warnings)
	}
	// An absent workspace root means the project layer contributes nothing,
	// but the embedded layer doesn't depend on it.
	want := []string{"done", "phase-impl", "phase-merge", "phase-plan", "phase-review"}
	for _, root := range []string{workspaceRoot, ""} {
		var ids []string
		for _, def := range Load(testEnv(), root).Definitions {
			if def.Source != templates.SourceEmbedded {
				t.Fatalf("Load(%q) holds %+v, want only embedded defaults", root, def)
			}
			ids = append(ids, def.ID)
		}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Fatalf("Load(%q) ids = %v, want the shipped defaults %v", root, ids, want)
		}
	}
}

func TestLoadMalformedFileWarnsAndSkipsWithoutBreakingOthers(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "good.md", "---\nevents: [show_backlog]\n---\ngood")
	writeWorkflow(t, project, "broken.md", "---\nlabels: 17\n---\nbad")

	set := Load(testEnv(), workspaceRoot)
	if len(authored(set)) != 1 {
		t.Fatalf("Definitions = %+v, want the good file beside the embedded defaults", set.Definitions)
	}
	if _, ok := findDefinition(set, "good"); !ok {
		t.Fatalf("Definitions = %+v, want \"good\" to have loaded", set.Definitions)
	}
	if len(set.Warnings) != 1 || set.Warnings[0].Path != "broken.md" {
		t.Fatalf("Warnings = %v, want one warning for broken.md", set.Warnings)
	}
}

func TestLoadInertFileLoadsWithWarningAndNeverFires(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "readme.md", "no frontmatter, just prose")

	set := Load(testEnv(), workspaceRoot)
	readme, ok := findDefinition(set, "readme")
	if !ok || !readme.Inert() {
		t.Fatalf("Definitions = %+v, want the inert definition loaded", set.Definitions)
	}
	if len(set.Warnings) != 1 || !strings.Contains(set.Warnings[0].Message, "inert") {
		t.Fatalf("Warnings = %v, want one inert warning", set.Warnings)
	}
	// The inert definition fires nowhere, whatever the embedded defaults
	// legitimately match alongside it.
	for _, event := range Catalog() {
		for _, matched := range set.Matching(Occasion{Event: event}) {
			if matched.ID == "readme" {
				t.Fatalf("Matching(%s) matched the inert definition, want it to fire nowhere", event)
			}
		}
	}
}
