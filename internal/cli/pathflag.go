package cli

import (
	"fmt"
	"strings"
)

// pathFlag is a flag whose value names a file or directory the caller chose.
// parseLeaf resolves it against the invocation's directory, so a relative path
// names what it named where the caller ran lit, whatever directory the process
// itself is in. Whitespace around the value is not part of the path. raw is
// the value as given, for the checks of its spelling and the messages that
// quote it back.
// [LAW:one-type-per-behavior] every path-valued flag is an instance of this.
type pathFlag struct {
	name string
	raw  *string
	abs  string
}

func registerPath(fs *cobraFlagSet, name, usage string) *pathFlag {
	f := &pathFlag{name: name, raw: fs.String(name, "", usage)}
	fs.paths = append(fs.paths, f)
	return f
}

// path is the absolute path the flag names, or "" when it was not given.
func (f *pathFlag) path() string {
	return f.abs
}

// resolvePaths resolves every path flag the leaf declares, once its flags have
// parsed. parseLeaf runs it for every leaf, so no command opens a path it was
// given against the process's directory. [LAW:single-enforcer]
func resolvePaths(fs *cobraFlagSet, dir WorkingDir) error {
	for _, f := range fs.paths {
		// An absent flag names no path; its value is the empty default.
		if !fs.Changed(f.name) {
			continue
		}
		abs, err := resolveFlagPath(dir, f.name, *f.raw)
		if err != nil {
			return err
		}
		f.abs = abs
	}
	return nil
}

// resolveFlagPath is the absolute path a flag's value names, by the one rule
// every path given to a flag follows: whitespace around it is not part of it,
// and an empty or flag-shaped value names no path. pflag takes any string as a
// value, so `--at --help` hands over "--help"; resolving it would open a path
// nobody meant, and resolving "" would open the working directory itself.
// [LAW:single-enforcer] [LAW:parse-dont-validate]
func resolveFlagPath(dir WorkingDir, flag, raw string) (string, error) {
	given := strings.TrimSpace(raw)
	if given == "" || strings.HasPrefix(given, "-") {
		return "", UsageError{Message: fmt.Sprintf("--%s needs a path, got %q", flag, raw)}
	}
	abs, err := dir.resolve(given)
	if err != nil {
		return "", fmt.Errorf("resolve --%s %s: %w", flag, raw, err)
	}
	return abs, nil
}
