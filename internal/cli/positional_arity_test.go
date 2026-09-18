package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// unboundedArityCommands are the leaves that legitimately take any number of
// positionals (`rank set` takes a whole id sequence, `stores` a list of roots).
// They are named here rather than inferred, because "this leaf refused nothing"
// and "this leaf accepts everything" render identically from outside and only
// the declaration tells them apart. A leaf added to this list without an
// allPositionals declaration would be an exemption granted to a bug.
var unboundedArityCommands = map[string]bool{
	"rank set": true,
	"stores":   true,
}

// leafPaths walks the registry and returns every invocable command path: a
// top-level command with no subcommands is itself a leaf, and a family
// contributes the full path of each of its nested subcommands.
// [LAW:one-source-of-truth] the registry is the only enumeration; a test that
// restated the command list would pass while blind to a newly added command,
// which is the failure this test exists to prevent.
func leafPaths(t *testing.T) [][]string {
	t.Helper()
	specs := commandSpecs(context.Background(), io.Discard, io.Discard)
	var paths [][]string
	var walk func(prefix []string, subs []SubcommandSpec)
	walk = func(prefix []string, subs []SubcommandSpec) {
		if len(subs) == 0 {
			paths = append(paths, prefix)
			return
		}
		for _, sub := range subs {
			walk(append(append([]string{}, prefix...), sub.Name), sub.Subcommands)
		}
	}
	for _, spec := range specs {
		if spec.Retired {
			// A retired command answers with its replacement pointer before any
			// arity check, which is the documented behaviour tested elsewhere.
			continue
		}
		walk([]string{spec.Name}, spec.Subcommands)
	}
	if len(paths) == 0 {
		t.Fatal("registry produced no command paths; the walk is broken, not the CLI")
	}
	return paths
}

// Every leaf refuses a positional it did not declare, rather than silently
// ignoring it. This is the property links-cli-errors-rl4s was filed for: the
// count check used to be hand-written inside each leaf's work, about thirty
// times, so the leaves that omitted it (`lit new ... stray`, `lit export stray`)
// exited 0 having dropped part of the command line. parseLeaf now enforces it
// once for every leaf. [LAW:single-enforcer] [LAW:no-silent-failure]
//
// The refusal precedes acquisition, so this runs with no workspace at all: a
// command that reached a store before noticing the stray token would fail here
// with a workspace error instead of a usage error, which is itself the
// regression worth catching.
func TestEveryLeafRefusesAnUndeclaredPositional(t *testing.T) {
	t.Parallel()
	// More surplus tokens than any bounded leaf declares (the widest is 2), so
	// one invocation covers every arity without the test needing to know each
	// leaf's count.
	surplus := []string{"zzz-stray-a", "zzz-stray-b", "zzz-stray-c"}

	checked := 0
	for _, path := range leafPaths(t) {
		name := strings.Join(path, " ")
		if unboundedArityCommands[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := Run(context.Background(), &out, &out, append(append([]string{}, path...), surplus...))
			if err == nil {
				t.Fatalf("lit %s <3 stray args> exited 0; the surplus was silently dropped", name)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("lit %s <3 stray args> exit = %d (%v), want %d (usage)", name, got, err, ExitUsage)
			}
			if msg := err.Error(); !strings.Contains(msg, "zzz-stray") {
				t.Errorf("lit %s error = %q, want it to name the offending token(s)", name, msg)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no commands were checked; the exemption list or the walk swallowed the whole registry")
	}
	t.Logf("checked %d leaf commands", checked)
}

// The enforcer itself, at the seam rather than through a command: a leaf that
// declares N positionals keeps N and refuses the N+1th, and the message names
// both the allowance and the offending token.
func TestParseLeafRefusesSurplusPositionals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		declared    int
		args        []string
		wantKept    []string
		wantRefused bool
	}{
		{name: "zero declared, one given", declared: 0, args: []string{"extra"}, wantRefused: true},
		{name: "one declared, one given", declared: 1, args: []string{"a"}, wantKept: []string{"a"}},
		{name: "one declared, two given", declared: 1, args: []string{"a", "b"}, wantRefused: true},
		{name: "two declared, two given", declared: 2, args: []string{"a", "b"}, wantKept: []string{"a", "b"}},
		{name: "unbounded declared, many given", declared: allPositionals, args: []string{"a", "b", "c"}, wantKept: []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newCobraFlagSet("probe")
			l := leaf[struct{}]{fs: fs, positionals: tc.declared}
			got, err := parseLeaf(l, tc.args, io.Discard)
			if tc.wantRefused {
				if err == nil {
					t.Fatalf("parseLeaf(declared=%d, %q) = %q, nil; want a usage refusal", tc.declared, tc.args, got)
				}
				var usage UsageError
				if !errors.As(err, &usage) {
					t.Fatalf("parseLeaf error = %v (%T), want UsageError", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLeaf(declared=%d, %q) error = %v, want the positionals kept", tc.declared, tc.args, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantKept, ",") {
				t.Errorf("parseLeaf positionals = %q, want %q", got, tc.wantKept)
			}
		})
	}
}

// A boolean flag no longer swallows the positional that follows it. splitArgs
// used to infer a flag's arity from whether the NEXT token had a leading dash,
// which is a fact about the argument rather than about the flag; it now asks the
// flag set, which is the only thing that knows. The regression this guards is
// user-visible: `lit prefix set --apply demo` refused itself as malformed
// because `demo` had been eaten as --apply's value.
func TestBooleanFlagDoesNotSwallowFollowingPositional(t *testing.T) {
	t.Parallel()
	fs := newCobraFlagSet("probe")
	apply := fs.Bool("apply", false, "apply")

	got, err := parseLeaf(leaf[struct{}]{fs: fs, positionals: 1}, []string{"--apply", "demo"}, io.Discard)
	if err != nil {
		t.Fatalf("parseLeaf(--apply demo) error = %v, want the positional kept", err)
	}
	if len(got) != 1 || got[0] != "demo" {
		t.Fatalf("parseLeaf(--apply demo) positionals = %q, want [demo]", got)
	}
	if !*apply {
		t.Error("--apply did not register as set")
	}
	// The other direction is unchanged: a flag that really does take a value
	// still consumes the token after it.
	fs2 := newCobraFlagSet("probe")
	value := fs2.String("value", "", "value")
	got2, err := parseLeaf(leaf[struct{}]{fs: fs2, positionals: 1}, []string{"--value", "v", "pos"}, io.Discard)
	if err != nil {
		t.Fatalf("parseLeaf(--value v pos) error = %v", err)
	}
	if *value != "v" {
		t.Errorf("--value = %q, want v", *value)
	}
	if len(got2) != 1 || got2[0] != "pos" {
		t.Errorf("parseLeaf(--value v pos) positionals = %q, want [pos]", got2)
	}
}
