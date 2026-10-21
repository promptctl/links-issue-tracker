// Package environ is the environment one lit run was given: its KEY=VALUE
// list, read the way the os package reads the process's own. cmd/lit's main
// builds it from os.Environ once, and everything below takes it as a
// parameter, so no code a run can reach reads the process's environment and
// two runs in one process cannot see each other's.
// [LAW:effects-at-boundaries] [LAW:no-shared-mutable-globals]
package environ

import (
	"errors"
	"runtime"
	"slices"
	"strings"
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

// TempDir is the directory for temporary files, resolved as os.TempDir
// resolves it on this GOOS. On windows that is the first of TMP, TEMP,
// USERPROFILE and SystemRoot that is set, the order GetTempPath2 tries them
// in, and an environment with none of them is refused.
func (e Env) TempDir() (string, error) {
	switch e.goos {
	case "windows":
		for _, key := range []string{"TMP", "TEMP", "USERPROFILE", "SystemRoot"} {
			if dir := e.Get(key); dir != "" {
				return dir, nil
			}
		}
		return "", errors.New("none of TMP, TEMP, USERPROFILE or SystemRoot is defined")
	case "plan9":
		return "/tmp", nil
	}
	if dir := e.Get("TMPDIR"); dir != "" {
		return dir, nil
	}
	return "/tmp", nil
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

// List is the environment as os/exec's Cmd.Env and gitcmd's Spec.Env take it.
// It is never nil: both read a nil list as "inherit the process", which is
// the read this package exists to end, so the empty environment is an empty,
// non-nil list.
func (e Env) List() []string {
	return append([]string{}, e.vars...)
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
