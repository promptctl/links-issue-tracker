package cli

// The CLI flag-parsing framework wrapper: cobraFlagSet adapts Cobra's pflag
// surface to the flag.FlagSet-shaped API every command handler declares its
// flags against, parseFlagSet is the one parse path (help rendering, retired
// flag interception, unknown-flag classification), and splitArgs separates
// positionals from flag tokens ahead of that parse. Nothing here knows any
// command's business logic; handlers compose these.
//
// [LAW:decomposition] Split out of cli.go (links-store-mb6e.6) so the
// flag-parsing framework, the business command handlers, and the typed error
// taxonomy no longer grow in one file.

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var errHelpHandled = errors.New("help handled")

type cobraFlagSet struct {
	cmd *cobra.Command
	// detail is the command's long-form description, rendered above the flag
	// table by printHelp. It is the one home for a command's explanatory help:
	// the binary carries the text, so `lit import --help` answers the same
	// question in a consumer repo that it answers here. Empty for a command
	// whose flag table already says everything it has to say.
	// [LAW:one-source-of-truth] The help answer is carried whole by the value
	// that renders it; nothing points outward at a file to finish the sentence.
	detail string
}

func newCobraFlagSet(use string) *cobraFlagSet {
	cmd := &cobra.Command{
		Use:           use,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.InitDefaultHelpFlag()
	// Cobra writes that flag's text from cmd.Name(), which is the FIRST word of
	// Use — so every multi-word leaf described itself by its family: `rank set`
	// advertised "help for rank", `dep add` "help for dep". A leaf's name here is
	// its whole invocation path, which is what the caller typed and what the
	// surrounding "Usage of rank set:" line already says.
	cmd.Flags().Lookup("help").Usage = "help for " + use
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.Flags().SetOutput(io.Discard)
	return &cobraFlagSet{cmd: cmd}
}

func (fs *cobraFlagSet) SetOutput(w io.Writer) {
	fs.cmd.SetOut(w)
	fs.cmd.SetErr(w)
	fs.cmd.Flags().SetOutput(w)
}

func (fs *cobraFlagSet) Parse(args []string) error {
	return fs.cmd.ParseFlags(args)
}

func (fs *cobraFlagSet) String(name string, value string, usage string) *string {
	return fs.cmd.Flags().String(name, value, usage)
}

func (fs *cobraFlagSet) Bool(name string, value bool, usage string) *bool {
	return fs.cmd.Flags().Bool(name, value, usage)
}

func (fs *cobraFlagSet) Int(name string, value int, usage string) *int {
	return fs.cmd.Flags().Int(name, value, usage)
}

// StringArray declares a repeatable string flag: each occurrence appends one
// value, with no splitting on commas, so a value may itself contain any
// character (including the multi-line merged prose the reconcile resolve carries).
func (fs *cobraFlagSet) StringArray(name string, usage string) *[]string {
	return fs.cmd.Flags().StringArray(name, nil, usage)
}

// StringOptional declares a string flag whose value is `defaultIfAbsent` when
// the flag is not passed, `defaultIfPresent` when the flag is passed with no
// value (e.g. `--eject`), or the caller-supplied value otherwise.
func (fs *cobraFlagSet) StringOptional(name, defaultIfPresent, defaultIfAbsent, usage string) *string {
	p := fs.cmd.Flags().String(name, defaultIfAbsent, usage)
	fs.cmd.Flags().Lookup(name).NoOptDefVal = defaultIfPresent
	return p
}

func (fs *cobraFlagSet) NArg() int {
	return fs.cmd.Flags().NArg()
}

func (fs *cobraFlagSet) Arg(i int) string {
	return fs.cmd.Flags().Arg(i)
}

func (fs *cobraFlagSet) Visit(fn func(*pflag.Flag)) {
	fs.cmd.Flags().Visit(fn)
}

func (fs *cobraFlagSet) Changed(name string) bool {
	return fs.cmd.Flags().Changed(name)
}

// Hide marks a flag as hidden so it does not appear in help output. The flag
// itself remains functional for any caller that still passes it explicitly.
func (fs *cobraFlagSet) Hide(name string) {
	_ = fs.cmd.Flags().MarkHidden(name)
}

// Detail declares the command's long-form description, next to the flags it
// describes. Returning the set keeps it chainable onto newCobraFlagSet at the
// top of a leaf declaration.
func (fs *cobraFlagSet) Detail(text string) *cobraFlagSet {
	fs.detail = text
	return fs
}

func (fs *cobraFlagSet) printHelp(helpOutput io.Writer) error {
	fs.SetOutput(helpOutput)
	if _, writeErr := io.WriteString(helpOutput, helpPage(fs.detail, fmt.Sprintf("Usage of %s:", fs.cmd.Use))); writeErr != nil {
		return writeErr
	}
	fs.cmd.Flags().PrintDefaults()
	return nil
}

func parseFlagSet(fs *cobraFlagSet, args []string, stdout io.Writer) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			// [LAW:single-enforcer] Flag help rendering is normalized in one Cobra parser path.
			if helpErr := fs.printHelp(stdout); helpErr != nil {
				return helpErr
			}
			return errHelpHandled
		}
		// Past ErrHelp, pflag's Parse fails only with its four typed errors — an
		// unknown flag, a missing value, an invalid value, bad syntax — and every
		// one is the caller mis-writing the command line. So all of them are one
		// UsageError, the same answer the root FlagErrorFunc gives. Classifying by
		// message prefix instead left `-x`, `--limit abc` and `---x` as bare errors
		// that exited 1 with "Retry the command" (links-output-format-yxjs).
		// [LAW:types-are-the-program] [LAW:single-enforcer]
		//
		// The retired flag is matched on the name pflag parsed, so `--continue`
		// and `--continue=x` match while `--continuex` stays an unknown flag.
		var unknown *pflag.NotExistError
		if errors.As(err, &unknown) && unknown.GetSpecifiedName() == "continue" {
			return UnsupportedError{Message: "--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag"}
		}
		return UsageError{Message: err.Error()}
	}
	if helpFlag := fs.cmd.Flags().Lookup("help"); helpFlag != nil && helpFlag.Changed {
		// [LAW:single-enforcer] Parsed help flags follow the same Cobra help rendering path as explicit help errors.
		if helpErr := fs.printHelp(stdout); helpErr != nil {
			return helpErr
		}
		return errHelpHandled
	}
	return nil
}

// flagTakesValue answers whether a "-"-prefixed token consumes the NEXT token as
// its value. pflag already records that per flag — NoOptDefVal is non-empty
// exactly for the flags that do not — so this ASKS the flag set instead of
// inferring it from the shape of the following token.
// [LAW:one-source-of-truth] the flag set is the authority on its own flags'
// arity. splitArgs used to re-derive it from "the next token has no leading
// dash", which is a statement about the ARGUMENT and not about the FLAG, and it
// was wrong in both directions: it fed a boolean the positional that followed it
// (`lit prefix set --apply <prefix>` lost the prefix and refused itself as
// malformed), and it fed an optional-value flag a value pflag accepts only as
// `--flag=value`, so the token arrived where nothing expected it.
//
// `lit children --include-archived <id>` is NOT an instance, though an earlier
// draft of this comment and of the changelog both said it was. The mis-split
// happened there too, but the listing surface declared zero positionals and read
// its id back out of pflag's leftovers precisely to survive it, so the caller
// always got the right answer. Checked against the master binary, which prints
// the child. The surface stops needing that workaround now; it was never a bug
// the caller could see.
//
// It does NOT by itself fix `lit quickstart --eject all`. pflag's rule for an
// optional-value flag is the equals sign, so `all` is the topic no matter how
// argv is split, and the command is genuinely refused either way. What was wrong
// there was the SENTENCE — "quickstart <topic> takes no flags", said by a
// command with three — and that is fixed in quickstartLeaf, which now names
// `--eject=LIST`. Recorded because the first draft of this comment claimed the
// split fixed it, and the two binaries printed the same line.
// An unknown flag consumes nothing: pflag refuses it a moment later, and leaving
// the following token where the caller put it keeps that refusal about the flag
// actually mistyped. [LAW:no-silent-failure]
func (fs *cobraFlagSet) flagTakesValue(token string) bool {
	if strings.Contains(token, "=") {
		return false
	}
	flags := fs.cmd.Flags()
	var flag *pflag.Flag
	switch {
	case strings.HasPrefix(token, "---"):
		// Not a spelling pflag accepts. TrimLeft used to strip every dash, so
		// `---limit` resolved to the real --limit and consumed the next token as
		// its value; pflag then refused the token anyway, so nothing visible
		// broke, but the function answered about a flag the caller did not
		// write. [LAW:one-source-of-truth] answer about the token as written.
		return false
	case strings.HasPrefix(token, "--"):
		flag = flags.Lookup(strings.TrimPrefix(token, "--"))
	case len(token) == 2:
		flag = flags.ShorthandLookup(strings.TrimPrefix(token, "-"))
		// A cluster like `-abc` matches no arm and so consumes nothing. That is
		// correct for this binary and checked rather than assumed: lit declares
		// no value-taking shorthand at all (TestNoValueTakingShorthandExists),
		// and cobra's own -h is boolean. The first `-v <value>` anyone adds would
		// need this arm to understand clusters, which is why the test names the
		// condition instead of leaving it to be rediscovered.
	}
	if flag == nil {
		return false
	}
	return takesValue(flag)
}

// takesValue is the one predicate for "does this flag consume the next token".
// pflag records it as NoOptDefVal: non-empty exactly for the flags that stand
// alone (booleans, and optional-value flags that accept a value only as
// --flag=value). Two questions need this same fact — how splitArgs divides argv,
// and which flags an arity refusal should point a caller at — and they must
// never be able to disagree about a given flag.
// [LAW:one-source-of-truth] one reading of pflag's record, two callers.
func takesValue(flag *pflag.Flag) bool {
	return flag.NoOptDefVal == ""
}

// valueTakingFlagNames lists the flags this command accepts a VALUE for, sorted,
// excluding the help flag and anything hidden. It is what a caller who typed a
// value positionally was probably reaching for, and it is read off the flag set
// rather than restated: a leaf cannot forget to update it, and it cannot name a
// flag the command does not have. [LAW:one-source-of-truth]
func (fs *cobraFlagSet) valueTakingFlagNames() []string {
	var names []string
	fs.cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden || flag.Name == "help" || !takesValue(flag) {
			return
		}
		names = append(names, "--"+flag.Name)
	})
	sort.Strings(names)
	return names
}

// optionalValueFlagNames names the flags pflag reads a value from ONLY with an
// equals sign. They are recorded as NoOptDefVal non-empty — the same field
// takesValue reads — so they are exactly the flags the value-naming clause must
// NOT list: written with a space, the value is not a value at all. A boolean is
// the other NoOptDefVal shape and takes no value in any form, so it is excluded
// by its type rather than by a name list that would have to be maintained.
// [LAW:one-source-of-truth]
func (fs *cobraFlagSet) optionalValueFlagNames() []string {
	var names []string
	fs.cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden || flag.Name == "help" || takesValue(flag) || flag.Value.Type() == "bool" {
			return
		}
		names = append(names, "--"+flag.Name)
	})
	sort.Strings(names)
	return names
}

func splitArgs(args []string, positionalCount int, fs *cobraFlagSet) ([]string, []string) {
	// positionalCount is a CEILING, and an unbounded-arity leaf states it as
	// allPositionals — so it is not a capacity. argv is the real bound: no more
	// positionals can land here than there are tokens to put in them.
	// [LAW:types-are-the-program] the allocation asks the input how big it can
	// get instead of trusting a number that is allowed to mean "no limit".
	positionals := make([]string, 0, min(positionalCount, len(args)))
	flags := make([]string, 0, len(args))
	// A value-taking flag whose value was not consumed leaves the flag stream
	// expecting one, and the terminator is structure rather than data — so it
	// must never land in that position. pflag consumes whatever follows such a
	// flag unconditionally, dash or not, so a terminator left in the stream is
	// taken as the value: on master `lit new --title -- --topic topics` created
	// an issue titled "--". Withholding it lets pflag raise "flag needs an
	// argument", which names the flag. [LAW:no-silent-failure]
	awaitingValue := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			// POSIX end-of-flags: every token after this is a positional
			// whatever it looks like, so `lit show -- -x` asks for the issue
			// literally named "-x". The loop used to classify post-terminator
			// tokens by their leading dash like any other, which reordered them
			// ahead of the positionals — harmless while nothing checked the
			// count, and, once parseLeaf did, a refusal of a command line the
			// caller had written correctly. [LAW:no-silent-failure]
			if awaitingValue {
				// The flag before the terminator never got its value, so the
				// line is malformed AT THAT FLAG and nothing after it can be
				// classified meaningfully. Handing pflag the dangling flag by
				// itself makes it say so and name it. Emitting anything more —
				// the terminator, or the tokens past the ceiling that would sit
				// behind it — only gives pflag something to swallow as the
				// value instead. The first draft of this guard withheld only the
				// terminator and still emitted the overflow, so
				// `lit comment add --body -- <id> hello` wrote a comment bodied
				// "hello" — a guard failing open into a write. That shape was
				// never released; it is recorded because the narrow fix looked
				// complete and the test pinned exactly the case it handled.
				// [LAW:no-silent-failure]
				return positionals, flags
			}
			flags = append(flags, arg)
			for _, rest := range args[index+1:] {
				if len(positionals) < positionalCount {
					positionals = append(positionals, rest)
					continue
				}
				// Past the ceiling it stays in the stream behind the terminator,
				// where pflag keeps it as a leftover and the arity refusal names
				// it — the same answer a surplus positional gets anywhere else.
				flags = append(flags, rest)
			}
			return positionals, flags
		}
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			// The leading-dash test on the NEXT token stays, but not because
			// pflag refuses a dash-leading value — it does not. pflag consumes
			// the next token as a value-required flag's value unconditionally,
			// so `lit upgrade --to --help` sets --to to the literal "--help"
			// and fetches a release tagged `v--help`; only `ls`/`children`
			// guard that shape themselves. The test stays because withholding
			// such a token here leaves it where the caller put it, so the
			// refusal stays about the flag actually mistyped. Only the boolean
			// case changes, and in one direction — tokens that used to be
			// swallowed are now left as positionals for the arity check to judge.
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") && fs.flagTakesValue(arg) {
				flags = append(flags, args[index+1])
				index++
				awaitingValue = false
				continue
			}
			awaitingValue = fs.flagTakesValue(arg)
			continue
		}
		if len(positionals) < positionalCount {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
	}
	return positionals, flags
}
