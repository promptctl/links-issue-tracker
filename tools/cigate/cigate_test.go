// Package cigate holds no code: its tests keep ci.yml's `gate` job the one
// list of what gates merge. Master's ruleset requires the single check named
// `gate`, so a ci.yml job gate does not need, or another workflow that runs on
// a PR, would report on the PR without blocking its merge. build-and-test runs
// these tests, so a PR that breaks the list fails before gate itself runs.
package cigate

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

const workflowsDir = "../../.github/workflows"

type job struct {
	Name  string    `yaml:"name"`
	If    string    `yaml:"if"`
	Needs yaml.Node `yaml:"needs"`
}

type workflow struct {
	On   yaml.Node      `yaml:"on"`
	Jobs map[string]job `yaml:"jobs"`
}

// readWorkflows parses every workflow file, keyed by base name.
func readWorkflows(t *testing.T) map[string]workflow {
	t.Helper()
	var paths []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(workflowsDir, pattern))
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		paths = append(paths, matches...)
	}
	workflows := map[string]workflow{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var wf workflow
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		workflows[filepath.Base(p)] = wf
	}
	if _, ok := workflows["ci.yml"]; !ok {
		t.Fatalf("no ci.yml among %d workflows in %s", len(workflows), workflowsDir)
	}
	return workflows
}

// scalars returns the values of a node written either as one scalar or as a
// sequence of scalars, the two shapes `needs:` and `on:` both accept.
func scalars(t *testing.T, n yaml.Node) []string {
	t.Helper()
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	}
	t.Fatalf("line %d: want a scalar or a sequence, got node kind %d", n.Line, n.Kind)
	return nil
}

// TestGateNeedsEveryOtherCIJob: a ci.yml job missing from gate's needs runs on
// every PR and blocks nothing. The `if` is pinned whole because `always()` is
// what turns a failed need into a failed gate; without it gate is skipped, and
// GitHub counts a skipped required check as passing.
func TestGateNeedsEveryOtherCIJob(t *testing.T) {
	ci := readWorkflows(t)["ci.yml"]
	gate, ok := ci.Jobs["gate"]
	if !ok {
		t.Fatal("ci.yml has no gate job")
	}

	var others []string
	for name := range ci.Jobs {
		if name != "gate" {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	needs := scalars(t, gate.Needs)
	slices.Sort(needs)
	if !slices.Equal(others, needs) {
		t.Errorf("gate's needs must name every other ci.yml job\n jobs:  %v\n needs: %v", others, needs)
	}

	const wantIf = "always() && github.event_name == 'pull_request'"
	if gate.If != wantIf {
		t.Errorf("gate's if is %q, want %q", gate.If, wantIf)
	}
}

// TestOnlyCIRunsOnPullRequests: a workflow outside ci.yml that runs on a PR
// reports a check gate cannot need. Each trigger is held to a list of what is
// allowed, so a trigger nobody thought of fails here [LAW:no-silent-failure].
func TestOnlyCIRunsOnPullRequests(t *testing.T) {
	masterOnly := map[string]any{"branches": []any{"master"}}
	for file, wf := range readWorkflows(t) {
		if file == "ci.yml" {
			continue
		}
		triggers := map[string]*yaml.Node{}
		if wf.On.Kind == yaml.MappingNode {
			for i := 0; i < len(wf.On.Content); i += 2 {
				triggers[wf.On.Content[i].Value] = wf.On.Content[i+1]
			}
		} else if wf.On.Kind != 0 {
			for _, name := range scalars(t, wf.On) {
				triggers[name] = nil
			}
		}
		if len(triggers) == 0 {
			t.Errorf("%s: no triggers read from `on:`", file)
		}

		for name, value := range triggers {
			switch name {
			case "schedule", "workflow_dispatch", "workflow_call":
			case "push":
				var filter any
				if value != nil {
					if err := value.Decode(&filter); err != nil {
						t.Fatalf("%s: decode on.push: %v", file, err)
					}
				}
				if !reflect.DeepEqual(filter, masterOnly) {
					t.Errorf("%s: push must be filtered to exactly branches: [master]; a wider push runs on PR branches outside gate (got %v)", file, filter)
				}
			default:
				t.Errorf("%s: trigger %q can run on a PR outside gate; make this a job of ci.yml so gate needs it", file, name)
			}
		}
	}
}

// TestGateIsTheOnlyCheckNamedGate: the ruleset requires a check by name, from
// any GitHub Actions job. Another job whose check is named `gate`, run on a
// PR's head commit, would answer for ci.yml's gate.
func TestGateIsTheOnlyCheckNamedGate(t *testing.T) {
	var named []string
	for file, wf := range readWorkflows(t) {
		for key, j := range wf.Jobs {
			check := key
			if j.Name != "" {
				check = j.Name
			}
			if check == "gate" {
				named = append(named, file+":"+key)
			}
		}
	}
	slices.Sort(named)
	if want := []string{"ci.yml:gate"}; !slices.Equal(named, want) {
		t.Errorf("jobs whose check is named gate: %v, want %v", named, want)
	}
}
