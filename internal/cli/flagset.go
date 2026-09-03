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
}

func newCobraFlagSet(use string) *cobraFlagSet {
	cmd := &cobra.Command{
		Use:           use,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.InitDefaultHelpFlag()
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

func (fs *cobraFlagSet) printHelp(helpOutput io.Writer) error {
	fs.SetOutput(helpOutput)
	if _, writeErr := fmt.Fprintf(helpOutput, "Usage of %s:\n", fs.cmd.Use); writeErr != nil {
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
		// [LAW:types-are-the-program] Wrap pflag errors at the parse boundary so sinks dispatch on type, not message text.
		msg := err.Error()
		if strings.Contains(msg, "flag provided but not defined: -output") ||
			strings.Contains(msg, "flag provided but not defined: --output") {
			return UnsupportedError{Message: "--output is no longer supported; omit it for text output", Feature: "--output"}
		}
		if strings.Contains(msg, "flag provided but not defined: -continue") ||
			strings.Contains(msg, "flag provided but not defined: --continue") ||
			strings.Contains(msg, "unknown flag: --continue") {
			return UnsupportedError{Message: "--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag", Feature: "--continue"}
		}
		if strings.HasPrefix(msg, "unknown flag:") || strings.HasPrefix(msg, "flag provided but not defined:") {
			return UsageError{Message: msg}
		}
		return err
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

func splitArgs(args []string, positionalCount int) ([]string, []string) {
	positionals := make([]string, 0, positionalCount)
	flags := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
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
