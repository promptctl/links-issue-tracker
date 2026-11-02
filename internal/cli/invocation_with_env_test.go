package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// withEnv changes the environment and nothing else. The fixture sets every
// field, so an Invocation that gains a field fails here until the fixture and
// withEnv both carry it, rather than dropping it from every run.
func TestWithEnvKeepsEveryOtherField(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	inv := Invocation{
		Dir:    WorkingDirOf("/work", nil),
		Args:   []string{"version"},
		Env:    environ.Of([]string{"A=1"}),
		Stdin:  strings.NewReader("in"),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	replacement := environ.Of([]string{"B=2"})
	got := inv.withEnv(replacement)

	in, out := reflect.ValueOf(inv), reflect.ValueOf(got)
	for i := range in.NumField() {
		name := in.Type().Field(i).Name
		if in.Field(i).IsZero() {
			t.Errorf("the fixture leaves Invocation.%s zero; set it so this test can see it dropped", name)
			continue
		}
		if name == "Env" {
			if v, _ := got.Env.Lookup("B"); v != "2" {
				t.Errorf("withEnv left Env = %v, want the replacement", got.Env.List())
			}
			continue
		}
		if !reflect.DeepEqual(in.Field(i).Interface(), out.Field(i).Interface()) {
			t.Errorf("withEnv changed Invocation.%s", name)
		}
	}
}
