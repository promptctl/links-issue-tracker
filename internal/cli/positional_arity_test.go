package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// unboundedArityCommands are the invocations that legitimately take any number
// of positionals. They are named here rather than inferred, because "this leaf
// refused nothing" and "this leaf accepts everything" render identically from
// outside and only the declaration tells them apart. A command added to this
// list without an unbounded declaration would be an exemption granted to a bug.
//
// The two entries are exempt for DIFFERENT reasons, which is worth saying since
// one list spelling two rules is how a list starts covering for a defect:
//   - "rank set" declares allPositionals and goes THROUGH the enforcer, which
//     admits every token by declaration.
//   - "stores" never reaches the enforcer at all: runStores calls parseFlagSet
//     directly and reads fs.Arg(i), so it has no declared ceiling to exceed.
//
// The first is a leaf that says "any"; the second is not a parseLeaf leaf.
var unboundedArityCommands = map[string]bool{
	"rank set": true,
	"stores":   true,
}

// bareFormsThatAreLeaves are family commands whose OWN bare invocation is a real
// leaf with declared arity, not a resolver that dispatches to children. They get
// the full assertion — a surplus positional must be named back — because for
// them a stray token really is a surplus positional. Every other family answers
// a first token it does not recognise with "that is not a subcommand", which is
// a different and equally correct refusal that names the alternatives instead.
var bareFormsThatAreLeaves = map[string]bool{
	"rank":       true,
	"bulk label": true,
}

// invocablePath is one invocable path plus whether it is a family's bare form.
// The distinction decides which assertion is true of it, so it travels WITH the
// path rather than being re-derived by the caller. [LAW:types-are-the-program]
type invocablePath struct {
	path     []string
	bareForm bool
}

// leafPaths walks the registry and returns every invocable command path.
//
// A family contributes the full path of each nested subcommand AND its own bare
// form, because the bare form is invocable too and is where some real leaves
// live. Emitting only the children is what let this test pass while blind:
// `rank` is registered with a single `set` subcommand, so the walk produced
// `rank set` and nothing else — and `rank set` is exempt as unbounded, so
// rankLeaf, which declares 1 positional, was exercised by NOTHING. The test that
// exists to prove every leaf refuses a surplus positional did not touch it, and
// changing its declaration to allPositionals would have kept the suite green.
// [LAW:one-source-of-truth] the registry is the only enumeration; a walk that
// skips a dispatchable path is a second, shorter map of the command surface.
func leafPaths(t *testing.T) []invocablePath {
	t.Helper()
	specs := commandSpecs(context.Background(), io.Discard, io.Discard)
	var paths []invocablePath
	var walk func(prefix []string, subs []SubcommandSpec)
	walk = func(prefix []string, subs []SubcommandSpec) {
		if len(subs) == 0 {
			paths = append(paths, invocablePath{path: prefix})
			return
		}
		// The family's own bare form, then each child.
		paths = append(paths, invocablePath{path: prefix, bareForm: true})
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

	checked, bare := 0, 0
	for _, cp := range leafPaths(t) {
		name := strings.Join(cp.path, " ")
		if unboundedArityCommands[name] {
			continue
		}
		namesToken := !cp.bareForm || bareFormsThatAreLeaves[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := Run(context.Background(), &out, &out, append(append([]string{}, cp.path...), surplus...))
			if err == nil {
				t.Fatalf("lit %s <3 stray args> exited 0; the surplus was silently dropped", name)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("lit %s <3 stray args> exit = %d (%v), want %d (usage)", name, got, err, ExitUsage)
			}
			msg := err.Error()
			if namesToken {
				// A stray token after a leaf is a surplus POSITIONAL, so the
				// refusal must hand it back — telling a caller the shape of the
				// command and leaving them to spot the difference is the defect
				// this ticket exists to remove.
				if !strings.Contains(msg, "zzz-stray") {
					t.Errorf("lit %s error = %q, want it to name the offending token(s)", name, msg)
				}
				return
			}
			// A stray token after a FAMILY is an unrecognised subcommand, and the
			// useful answer names the subcommands that do exist rather than
			// echoing what was typed. Assert it is not vacuous: it must at least
			// name the command the caller was reaching for.
			if !strings.Contains(msg, cp.path[len(cp.path)-1]) {
				t.Errorf("lit %s error = %q, want the family usage naming its subcommands", name, msg)
			}
		})
		checked++
		if cp.bareForm {
			bare++
		}
	}
	if checked == 0 {
		t.Fatal("no commands were checked; the exemption list or the walk swallowed the whole registry")
	}
	if bare == 0 {
		t.Fatal("no family bare forms were checked; the walk regressed to children-only and `rank` is invisible again")
	}
	t.Logf("checked %d command paths (%d of them family bare forms)", checked, bare)
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

// adaptLeaf must carry a leaf's WHOLE declaration across to a leaf over another
// resource type. The three pipeline adapters used to rebuild the struct inline,
// naming fs and positionals, and every one of them silently dropped `usage` the
// day that field appeared: `lit upgrade v0.9.0` answered with the generic
// allowance while `usage: lit upgrade [--to <version>]` sat in its declaration.
//
// The field-count tripwire is the half that survives the next change. Checking
// the three fields by name only proves today's fields are copied; a field added
// to leaf tomorrow would be absent from adaptLeaf AND absent from this test, and
// the drift would be invisible again. The count fails instead, and says what to
// do. [LAW:one-source-of-truth]
func TestAdaptLeafCarriesTheWholeDeclaration(t *testing.T) {
	t.Parallel()
	const fieldsAdaptLeafKnowsAbout = 4 // fs, positionals, usage, work
	if n := reflect.TypeOf(leaf[int]{}).NumField(); n != fieldsAdaptLeafKnowsAbout {
		t.Fatalf("leaf has %d fields, want %d: a field was added or removed — carry it in adaptLeaf, then update this count", n, fieldsAdaptLeafKnowsAbout)
	}

	fs := newCobraFlagSet("probe")
	from := leaf[int]{fs: fs, positionals: 2, usage: "usage: lit probe --from <id> --to <id>"}
	got := adaptLeaf[int, string](from, func(context.Context, io.Writer, string, []string) error { return nil })

	if got.fs != from.fs {
		t.Error("adaptLeaf dropped fs")
	}
	if got.positionals != from.positionals {
		t.Errorf("adaptLeaf positionals = %d, want %d", got.positionals, from.positionals)
	}
	if got.usage != from.usage {
		t.Errorf("adaptLeaf usage = %q, want %q", got.usage, from.usage)
	}
}

// The end of that story from outside: the adapted commands print the sentence
// their leaf declares, not the generic allowance.
func TestAdaptedCommandsKeepTheirUsageSentence(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := Run(context.Background(), &out, &out, []string{"upgrade", "v0.9.0"})
	if err == nil {
		t.Fatal("lit upgrade v0.9.0 exited 0")
	}
	if msg := err.Error(); !strings.Contains(msg, "[--to <version>]") {
		t.Errorf("lit upgrade v0.9.0 = %q, want the leaf's own `[--to <version>]` sentence", msg)
	}
}

// lit declares no shorthand that takes a value, which is what lets
// flagTakesValue answer `false` for a cluster like `-abc` without decomposing
// it. The condition is checked rather than assumed: the day someone adds
// `-l <label>`, `lit x -l value` would drop `value` into the positionals and the
// new arity refusal would reject a correct command line. This fails then, at the
// flag declaration, instead of at a user's terminal.
// [LAW:no-silent-failure] the assumption is a test, not a comment.
func TestNoValueTakingShorthandExists(t *testing.T) {
	t.Parallel()
	for _, cp := range leafPaths(t) {
		name := strings.Join(cp.path, " ")
		fs := newCobraFlagSet(name)
		var offenders []string
		fs.cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Shorthand != "" && f.Name != "help" && takesValue(f) {
				offenders = append(offenders, "-"+f.Shorthand+"/--"+f.Name)
			}
		})
		if len(offenders) > 0 {
			t.Errorf("lit %s declares value-taking shorthand(s) %v; teach flagTakesValue to decompose clusters before adding one", name, offenders)
		}
	}
}

// Everything after a POSIX `--` is a positional, whatever it looks like.
func TestTerminatorMakesEverythingAfterItPositional(t *testing.T) {
	t.Parallel()
	fs := newCobraFlagSet("probe")
	field := fs.String("field", "", "field")

	got, err := parseLeaf(leaf[struct{}]{fs: fs, positionals: 1}, []string{"--field", "f", "--", "-x"}, io.Discard)
	if err != nil {
		t.Fatalf("parseLeaf(--field f -- -x) error = %v, want -x kept as the positional", err)
	}
	if *field != "f" {
		t.Errorf("--field = %q, want f", *field)
	}
	if len(got) != 1 || got[0] != "-x" {
		t.Fatalf("positionals = %q, want [-x]", got)
	}

	// A surplus after the terminator is still a surplus, and is still named.
	fs2 := newCobraFlagSet("probe")
	_, err = parseLeaf(leaf[struct{}]{fs: fs2, positionals: 1}, []string{"--", "a", "b"}, io.Discard)
	var usage UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("parseLeaf(-- a b) error = %v (%T), want UsageError", err, err)
	}
	if !strings.Contains(usage.Error(), "b") {
		t.Errorf("error = %q, want it to name the surplus token b", usage.Error())
	}
}

// A leaf that says nothing still names the acts that work, because the fallback
// is derived from the flag set instead of hand-written onto each leaf.
func TestDerivedUsageNamesTheValueTakingFlags(t *testing.T) {
	t.Parallel()
	fs := newCobraFlagSet("probe")
	fs.String("path", "", "path")
	fs.Bool("force", false, "force")

	line := derivedUsage(fs, 0)
	if !strings.Contains(line, "--path") {
		t.Errorf("derivedUsage = %q, want it to name --path", line)
	}
	if strings.Contains(line, "--force") {
		t.Errorf("derivedUsage = %q, must not name --force: a boolean takes no value, so it is not what a stray value was meant to be", line)
	}

	// Past the readable handful it points at --help rather than reciting a table.
	wide := newCobraFlagSet("probe")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		wide.String(n, "", n)
	}
	if line := derivedUsage(wide, 0); !strings.Contains(line, "--help") {
		t.Errorf("derivedUsage with 5 value flags = %q, want it to point at --help", line)
	}
}
