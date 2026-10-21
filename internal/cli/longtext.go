package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

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
// pair, so no long-text field grows a second convention for reading its text
// from a file.
type longTextFlag struct {
	fs     *cobraFlagSet
	name   string
	inline *string
	file   *string
	// text is what the invocation supplied, nil when it supplied nothing:
	// readLongTexts fills it in the parse, before the workspace is opened, so
	// a pipe or a terminal that is slow to close never holds the workspace
	// lock, and a mistaken file is refused without opening a store.
	text *string
}

// textSource is where an invocation's long text came from, and the value its
// event records as text_source.<name>, so a body that landed wrong can be traced
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

func registerLongText(fs *cobraFlagSet, name, usage string) *longTextFlag {
	f := &longTextFlag{
		fs:     fs,
		name:   name,
		inline: fs.String(name, "", usage),
		file:   fs.String(name+"-file", "", fmt.Sprintf("Read --%s from this file, or from stdin when it is %s", name, stdinPath)),
	}
	fs.longTexts = append(fs.longTexts, f)
	return f
}

// readLongTexts reads every long-text field the leaf declares, once its flags
// have parsed. parseLeaf runs it for every leaf, so no command reads its text
// after the open. [LAW:single-enforcer] [LAW:no-ambient-temporal-coupling]
func readLongTexts(ctx context.Context, fs *cobraFlagSet, inv Invocation) error {
	for _, f := range fs.longTexts {
		text, err := f.read(ctx, inv)
		if err != nil {
			return err
		}
		f.text = text
	}
	return nil
}

// read returns the text the invocation supplied, or nil when it supplied
// none, so a command that writes only the fields it was given can tell an
// absent field from an emptied one. Supplying both flags is a usage error:
// neither one silently outranks the other. [LAW:no-silent-failure]
func (f *longTextFlag) read(ctx context.Context, inv Invocation) (*string, error) {
	source, err := f.source()
	if err != nil {
		return nil, err
	}
	telemetry.Set(ctx, "text_source."+f.name, string(source))
	var data []byte
	switch source {
	case textAbsent:
		return nil, nil
	case textInline:
		text := *f.inline
		return &text, nil
	case textStdin:
		if data, err = io.ReadAll(inv.Stdin); err != nil {
			return nil, fmt.Errorf("read --%s-file from stdin: %w", f.name, err)
		}
	default:
		// The file is not a pathFlag because "-" is not a path: it reads stdin
		// even where the directory cannot be read.
		path, resolveErr := inv.Dir.resolve(*f.file)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve --%s-file %s: %w", f.name, *f.file, resolveErr)
		}
		// A path that cannot be read is an argument to correct, not a failure
		// to retry, so it is refused as usage.
		if data, err = os.ReadFile(path); err != nil {
			return nil, UsageError{Message: fmt.Sprintf("read --%s-file: %v", f.name, err)}
		}
	}
	// A file or stdin that carries no text is a missing heredoc or pipe far
	// more often than a request for empty text, and an update would empty the
	// field with it, so it is refused; the inline flag gives empty text on
	// purpose. [LAW:no-silent-failure]
	text := string(data)
	if strings.TrimSpace(text) == "" {
		return nil, UsageError{Message: fmt.Sprintf("--%s-file %s supplied no text", f.name, *f.file)}
	}
	return &text, nil
}

// value is the supplied text, or "" when none was supplied: the value a
// create carries for a field it was not given.
func (f *longTextFlag) value() string {
	if f.text == nil {
		return ""
	}
	return *f.text
}

func (f *longTextFlag) source() (textSource, error) {
	inline, file := f.fs.Changed(f.name), f.fs.Changed(f.name+"-file")
	switch {
	case inline && file:
		return "", UsageError{Message: fmt.Sprintf("--%s and --%s-file both supply the same text; give one of them", f.name, f.name)}
	case inline:
		return textInline, nil
	case !file:
		return textAbsent, nil
	case !f.readsStdin():
		return textFile, nil
	}
	// stdin carries one text, so a second field reading it would get what the
	// first left: nothing. Every reader is named, so the refusal reads the same
	// whichever field resolves first.
	readers := []string{}
	for _, other := range f.fs.longTexts {
		if other.readsStdin() {
			readers = append(readers, "--"+other.name+"-file")
		}
	}
	if len(readers) > 1 {
		return "", UsageError{Message: fmt.Sprintf("only one field can read stdin, and %s each name it; give the others a path", strings.Join(readers, " and "))}
	}
	return textStdin, nil
}

func (f *longTextFlag) readsStdin() bool {
	return f.fs.Changed(f.name+"-file") && *f.file == stdinPath
}
