package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// Invocation is what one lit run takes from the process that started it: the
// directory it ran in, its arguments, its environment and its standard
// streams. The fields are named as os/exec.Cmd names the same things. cmd/lit's
// main reads the process once to build it, and a test builds its own, so
// nothing under internal/ reads the process's working directory, arguments,
// environment or stdin, and two runs in one process cannot see each other's.
// [LAW:effects-at-boundaries] [LAW:no-shared-mutable-globals]
type Invocation struct {
	Dir WorkingDir
	// Args are the arguments after the program name.
	Args   []string
	Env    environ.Env
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// WorkingDir is the absolute directory an invocation ran in, or the error
// reading it gave. An unreadable one fails only what needs it, through Path:
// the commands that resolve a workspace or a relative path refuse with it,
// on the invocation's own event, while `lit version` still answers.
// [LAW:parse-dont-validate] the failure is a value the caller has to unwrap.
type WorkingDir struct {
	path string
	err  error
}

// WorkingDirOf is the working directory os.Getwd reported, in the two values
// it returns them as. A path that is not absolute is refused here, as the
// error, because every relative path is resolved against it and a relative
// one would leave the process's own directory to finish the job.
// [LAW:parse-dont-validate]
func WorkingDirOf(path string, err error) WorkingDir {
	if err == nil && !filepath.IsAbs(path) {
		err = fmt.Errorf("working directory %q is not absolute", path)
	}
	return WorkingDir{path: path, err: err}
}

// Path is the directory, or why it could not be read. The zero WorkingDir is
// an invocation built without one, and it is refused as such rather than read
// as the empty path, which git and os would both take to mean the process's
// directory.
func (d WorkingDir) Path() (string, error) {
	if d == (WorkingDir{}) {
		return "", errors.New("the invocation was given no working directory")
	}
	return d.path, d.err
}

// resolve is the absolute path a caller-supplied path names: itself when
// absolute, otherwise joined to the directory, as os/exec resolves a relative
// Cmd.Path against Cmd.Dir. Only a relative path needs the directory, so only
// a relative path fails when it could not be read.
func (d WorkingDir) resolve(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	dir, err := d.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, path), nil
}

// serializeWrites is a run's stderr as every callee is handed it: one Write at
// a time, as os/exec.Cmd writes a child's shared streams. A routine store hold
// reports a failed heartbeat from its own goroutine while the command writes
// from the run's, and the writer a caller passed in owes neither of them that
// ordering. An *os.File already gives it — each Write holds the descriptor's
// write lock — and stays itself, because os/exec hands a file's descriptor to
// a child (the $EDITOR `lit workflows edit` opens) where any other writer gets
// a pipe that Wait drains until every process holding it has closed it.
// [LAW:single-enforcer] Run wraps it once.
func serializeWrites(w io.Writer) io.Writer {
	if file, ok := w.(*os.File); ok {
		return file
	}
	return &serialWriter{w: w}
}

// serialWriter lets one Write through to w at a time.
type serialWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *serialWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
