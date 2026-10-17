package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// longTextFlag is a free-text field long enough to span paragraphs — a
// comment body, a description, an agent prompt — declared as the pair gh
// declares for its own bodies: --<name> carries the text as one argument, and
// --<name>-file reads it from a file, or from stdin when the path is "-". The
// file form exists because a one-argument body is shell-quoting surface:
// every quote, backtick, dollar sign and newline in it is parsed by the shell
// before lit sees a byte.
// [LAW:one-type-per-behavior] every long-text field is one instance of this
// pair, so no command grows a second convention for reading text from a file.
type longTextFlag struct {
	fs     *cobraFlagSet
	name   string
	inline *string
	file   *string
}

// textSource is where an invocation's long text came from, and the value its
// event records as <name>.source, so a body that landed wrong can be traced
// to the input path that carried it. [LAW:nothing-unseen]
type textSource string

const (
	textAbsent textSource = "absent"
	textInline textSource = "inline"
	textFile   textSource = "file"
	textStdin  textSource = "stdin"
)

// stdinPath is the --<name>-file value that names stdin, as it does for gh.
const stdinPath = "-"

func registerLongText(fs *cobraFlagSet, name, usage string) longTextFlag {
	return longTextFlag{
		fs:     fs,
		name:   name,
		inline: fs.String(name, "", usage),
		file:   fs.String(name+"-file", "", fmt.Sprintf("Read --%s from this file, or from stdin when it is %s", name, stdinPath)),
	}
}

// resolve returns the text the invocation supplied, or nil when it supplied
// none, so a command that writes only the fields it was given can tell an
// absent field from an emptied one. Call it after parseFlagSet has run.
// Supplying both flags is a usage error: neither one silently outranks the
// other. [LAW:no-silent-failure]
func (f longTextFlag) resolve(ctx context.Context) (*string, error) {
	source, err := f.source()
	if err != nil {
		return nil, err
	}
	telemetry.Set(ctx, f.name+".source", string(source))
	switch source {
	case textAbsent:
		return nil, nil
	case textInline:
		text := *f.inline
		return &text, nil
	case textStdin:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read --%s-file from stdin: %w", f.name, err)
		}
		text := string(data)
		return &text, nil
	default:
		// A path that cannot be read is an argument to correct, not a failure
		// to retry, so it is refused as usage.
		data, err := os.ReadFile(*f.file)
		if err != nil {
			return nil, UsageError{Message: fmt.Sprintf("read --%s-file: %v", f.name, err)}
		}
		text := string(data)
		return &text, nil
	}
}

// value is the supplied text, or "" when none was supplied: the value a
// create carries for a field it was not given.
func (f longTextFlag) value(ctx context.Context) (string, error) {
	text, err := f.resolve(ctx)
	if err != nil || text == nil {
		return "", err
	}
	return *text, nil
}

func (f longTextFlag) source() (textSource, error) {
	inline, file := f.fs.Changed(f.name), f.fs.Changed(f.name+"-file")
	switch {
	case inline && file:
		return "", UsageError{Message: fmt.Sprintf("--%s and --%s-file both supply the same text; give one of them", f.name, f.name)}
	case inline:
		return textInline, nil
	case !file:
		return textAbsent, nil
	case *f.file == stdinPath:
		return textStdin, nil
	default:
		return textFile, nil
	}
}
