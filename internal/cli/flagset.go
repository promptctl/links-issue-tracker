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
// (`lit children --include-archived <id>` lost the id), and it fed `--eject all`
// a value that pflag accepts only as `--eject=all`, which surfaced as the
// unrelated "quickstart <topic> takes no flags".
// An unknown flag consumes nothing: pflag refuses it a moment later, and leaving
// the following token where the caller put it keeps that refusal about the flag
// actually mistyped. [LAW:no-silent-failure]
func (fs *cobraFlagSet) flagTakesValue(token string) bool {
	if strings.Contains(token, "=") {
		return false
	}
	name := strings.TrimLeft(token, "-")
	if name == "" {
		return false
	}
	flags := fs.cmd.Flags()
	var flag *pflag.Flag
	switch {
	case strings.HasPrefix(token, "--"):
		flag = flags.Lookup(name)
	case len(name) == 1:
		flag = flags.ShorthandLookup(name)
	}
	if flag == nil {
		return false
	}
	return flag.NoOptDefVal == ""
}

func splitArgs(args []string, positionalCount int, fs *cobraFlagSet) ([]string, []string) {
	// positionalCount is a CEILING, and an unbounded-arity leaf states it as
	// allPositionals — so it is not a capacity. argv is the real bound: no more
	// positionals can land here than there are tokens to put in them.
	// [LAW:types-are-the-program] the allocation asks the input how big it can
	// get instead of trusting a number that is allowed to mean "no limit".
	positionals := make([]string, 0, min(positionalCount, len(args)))
	flags := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			// The leading-dash test on the NEXT token stays: a value-taking flag
			// written `--at --help` still reaches pflag with its value missing,
			// which is the refusal that shape already earned. Only the boolean
			// case changes, and it changes in one direction — tokens that used
			// to be swallowed are now left as positionals for the arity check
			// below to judge.
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") && fs.flagTakesValue(arg) {
				flags = append(flags, args[index+1])
				index++
			}
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
