package cli

import (
	"fmt"
	"os"
)

// inputFileFlag is a path flag naming a file whose contents are the command's
// input — an import spec, a recovery mapping, an export to restore. parseLeaf
// reads it once the path resolves, before the workspace is opened, so a
// mistaken path is refused as usage without opening a store, as a long-text
// file is.
// [LAW:one-type-per-behavior] every flag that names a file to consume is one
// instance of this.
type inputFileFlag struct {
	path *pathFlag
	// file is what the flag named, nil when it was absent: one field, so an
	// absent flag cannot be confused with an empty file.
	// [LAW:types-are-the-program]
	file *inputFile
}

// inputFile is a file an invocation supplied as input: where it is, and what it
// held when it was read.
type inputFile struct {
	path string
	data []byte
}

func registerInputFile(fs *cobraFlagSet, name, usage string) *inputFileFlag {
	f := &inputFileFlag{path: registerPath(fs, name, usage)}
	fs.inputFiles = append(fs.inputFiles, f)
	return f
}

// input is the file the flag named, or nil when the flag was absent.
func (f *inputFileFlag) input() *inputFile {
	return f.file
}

// readInputFiles reads every input file the leaf declares, once its paths have
// resolved. parseLeaf runs it for every leaf, so no command reads its input
// file after the open. [LAW:single-enforcer] [LAW:no-ambient-temporal-coupling]
func readInputFiles(fs *cobraFlagSet) error {
	for _, f := range fs.inputFiles {
		// An absent flag names no file; resolvePaths left its path empty.
		if f.path.path() == "" {
			continue
		}
		data, err := readFlagFile(f.path.name, f.path.path())
		if err != nil {
			return err
		}
		f.file = &inputFile{path: f.path.path(), data: data}
	}
	return nil
}

// readFlagFile reads the file a flag named. A path that cannot be read is an
// argument to correct, not a failure to retry, so it is refused as usage.
// [LAW:single-enforcer] the one place a flag's unreadable file is refused, for
// input files and long-text files alike.
func readFlagFile(flag, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, UsageError{Message: fmt.Sprintf("read --%s: %v", flag, err)}
	}
	return data, nil
}
