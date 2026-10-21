// Package environ is the environment one lit run was given: its KEY=VALUE
// list, read the way the os package reads the process's own. cmd/lit's main
// builds it from os.Environ once, and everything below takes it as a
// parameter, so no code a run can reach reads the process's environment and
// two runs in one process cannot see each other's.
// [LAW:effects-at-boundaries] [LAW:no-shared-mutable-globals]
package environ

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
)

// Env is an environment, as os.Environ lists one. The zero Env is the empty
// environment, which `env -i` runs a program in.
type Env struct {
	vars []string
	goos string
}

// Of is the environment list names, in os.Environ's KEY=VALUE form.
func Of(list []string) Env {
	return of(list, runtime.GOOS)
}

func of(list []string, goos string) Env {
	return Env{vars: slices.Clone(list), goos: goos}
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
	return Env{vars: kept, goos: e.goos}
}

// With is the environment with the KEY=VALUE entries set appended, so each
// overrides any earlier entry for its key.
func (e Env) With(set ...string) Env {
	return Env{vars: append(slices.Clone(e.vars), set...), goos: e.goos}
}

// List is the environment as a KEY=VALUE list, a copy of the Env's own. It is
// never nil: os/exec and gitcmd read a nil list as "inherit the process",
// which is the read this package exists to end.
func (e Env) List() []string {
	return append([]string{}, e.vars...)
}

// Command, CommandContext and Git are the one way code a run can reach builds
// a child process: each runs the child in this environment, so no caller sets
// Env itself or can leave it nil. TestNothingRunReachesReadsTheProcess refuses
// every other way to build one. [LAW:single-enforcer]

// Command is exec.Command run in this environment.
func (e Env) Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = e.List()
	return cmd
}

// CommandContext is exec.CommandContext run in this environment.
func (e Env) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e.List()
	return cmd
}

// Git is gitcmd.Command run in this environment, whatever Env spec carries.
func (e Env) Git(ctx context.Context, spec gitcmd.Spec, args ...string) *gitcmd.Cmd {
	spec.Env = e.List()
	return gitcmd.Command(ctx, spec, args...)
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
