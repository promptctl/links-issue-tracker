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
		// An absent flag names no path, and resolving "" would turn it into the
		// working directory.
		given := strings.TrimSpace(*f.raw)
		if given == "" {
			continue
		}
		abs, err := dir.resolve(given)
		if err != nil {
			return fmt.Errorf("resolve --%s %s: %w", f.name, *f.raw, err)
		}
		f.abs = abs
	}
	return nil
}
