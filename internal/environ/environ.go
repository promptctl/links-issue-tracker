// Package environ is the environment one lit run was given: its KEY=VALUE
// list, read the way the os package reads the process's own. cmd/lit's main
// builds it from os.Environ once, and everything below takes it as a
// parameter, so no code of this module a run can reach reads the process's
// environment and two runs in one process cannot see each other's. Two reads
// stay the process's: the PATH os/exec finds a child's program on, and
// Dolt's own, which include the environment of the git it launches for a
// git remote.
// [LAW:effects-at-boundaries] [LAW:no-shared-mutable-globals]
package environ

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
)

// Env is an environment, as os.Environ lists one. The zero Env is no
// environment at all, the value a literal that leaves its Env out holds: its
// methods read it as the empty one, and cli.Run refuses an invocation that
// carries it. Of(nil) is the empty environment, which `env -i` runs a
// program in.
type Env struct {
	vars []string
	goos string
	// gitSpawns counts the git processes Git has started for this Env and
	// every Env derived from it. [LAW:no-shared-mutable-globals] Its owner is
	// the lineage one Of starts — one run's environment — so two runs in one
	// process never share it; Git is its only writer and GitSpawns its reader.
	gitSpawns *atomic.Int64
}

// IsZero reports whether e is the zero Env, which no Env built by Of or
// derived from one is.
func (e Env) IsZero() bool {
	return e.goos == ""
}

// Of is the environment list names, in os.Environ's KEY=VALUE form.
func Of(list []string) Env {
	return of(list, runtime.GOOS)
}

func of(list []string, goos string) Env {
	return Env{vars: slices.Clone(list), goos: goos, gitSpawns: new(atomic.Int64)}
}

// Lookup is the value of key, as os.LookupEnv reports it: the last entry for
// the key wins, as os/exec settles duplicates, and on windows the key matches
// without regard to case.
func (e Env) Lookup(key string) (string, bool) {
	for i := len(e.vars) - 1; i >= 0; i-- {
		if name, value := split(e.vars[i]); e.sameKey(name, key) {
			return value, true
		}
	}
	return "", false
}

// Get is the value of key, or "" when it is unset, as os.Getenv reports it.
func (e Env) Get(key string) string {
	value, _ := e.Lookup(key)
	return value
}

// HomeDir is the user's home directory, resolved as os.UserHomeDir resolves
// it on this GOOS.
func (e Env) HomeDir() (string, error) {
	key, spelled := "HOME", "$HOME"
	switch e.goos {
	case "windows":
		key, spelled = "USERPROFILE", "%userprofile%"
	case "plan9":
		key, spelled = "home", "$home"
	}
	if home := e.Get(key); home != "" {
		return home, nil
	}
	return "", errors.New(spelled + " is not defined")
}

// Without is the environment with every entry for the named keys removed.
func (e Env) Without(keys ...string) Env {
	kept := make([]string, 0, len(e.vars))
	for _, kv := range e.vars {
		name, _ := split(kv)
		if !slices.ContainsFunc(keys, func(key string) bool { return e.sameKey(name, key) }) {
			kept = append(kept, kv)
		}
	}
	return Env{vars: kept, goos: e.goos, gitSpawns: e.gitSpawns}
}

// With is the environment with the KEY=VALUE entries set appended, so each
// overrides any earlier entry for its key.
func (e Env) With(set ...string) Env {
	return Env{vars: append(slices.Clone(e.vars), set...), goos: e.goos, gitSpawns: e.gitSpawns}
}

// List is the environment as a KEY=VALUE list, a copy of the Env's own. It is
// never nil: os/exec and gitcmd read a nil list as "inherit the process",
// which is the read this package exists to end.
func (e Env) List() []string {
	return append([]string{}, e.vars...)
}

// Command, CommandContext and Git are the one way code a run can reach builds
// a child process: each runs the child in dir ("" is the caller's) in this
// environment, so no caller sets Env itself or can leave it nil.
// TestNothingRunReachesReadsTheProcess refuses every other way to build one.
// [LAW:single-enforcer]

// Command is exec.Command run in dir in this environment.
func (e Env) Command(dir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, e.in(dir)
	return cmd
}

// CommandContext is exec.CommandContext run in dir in this environment.
func (e Env) CommandContext(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, e.in(dir)
	return cmd
}

// Git is gitcmd.Command run in dir on stdin (nil is none) in this
// environment. It takes no gitcmd.Spec, whose Env a caller could set only to
// have it replaced: the environment is this Env, whole, and adding to it is
// With's job.
//
// Every git lit itself starts is started here, so this is where they are
// counted (GitSpawns); Dolt's own git, inside a fetch or push, is started by
// Dolt and is not. The zero Env has no counter, and Run refuses it before any
// git can start. [LAW:nothing-unseen] [LAW:single-enforcer]
func (e Env) Git(ctx context.Context, dir string, stdin io.Reader, args ...string) *gitcmd.Cmd {
	e.gitSpawns.Add(1)
	return gitcmd.Command(ctx, gitcmd.Spec{Dir: dir, Env: e.in(dir), Stdin: stdin}, args...)
}

// GitSpawns is how many git processes Git has started for this Env's lineage.
// The zero Env, which is no environment, has started none.
func (e Env) GitSpawns() int64 {
	if e.IsZero() {
		return 0
	}
	return e.gitSpawns.Load()
}

// in is the list a child started in dir runs with: this Env's, with PWD
// naming dir, as os/exec adds it for a child that inherits the process, so
// the child's $PWD is the directory it runs in and not lit's. "" is the
// caller's own directory, for which os/exec adds none either, and a relative
// dir could be made absolute only from the process's working directory;
// windows and plan9 get no PWD, as os/exec gives them none.
func (e Env) in(dir string) []string {
	list := e.List()
	if filepath.IsAbs(dir) && e.goos != "windows" && e.goos != "plan9" {
		list = append(list, "PWD="+filepath.Clean(dir))
	}
	return list
}

func (e Env) sameKey(a, b string) bool {
	if e.goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// split is an entry's key and value. A windows environment holds entries such
// as "=C:=C:\dir", whose key starts with the "=", so the key ends at the first
// "=" after its first byte, as os/exec reads it.
func split(kv string) (key, value string) {
	if i := strings.Index(kv[min(1, len(kv)):], "="); i >= 0 {
		i += min(1, len(kv))
		return kv[:i], kv[i+1:]
	}
	return kv, ""
}
