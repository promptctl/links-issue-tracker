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

	"github.com/promptctl/links-issue-tracker/internal/version"
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
	// the lineage one Of starts, which cmd/lit starts once per run; a caller
	// that hands one Env to several runs shares the count among them. Git is
	// its only writer and GitSpawns its reader.
	gitSpawns *atomic.Int64
	// releasedSchema is the highest schema version a published release
	// carries, as cmd/lit read it from its build stamp
	// (version.ReadReleasedSchemaCeiling). It is no environment variable: a
	// variable could be set to lift it. It rides here because Env is the one
	// value cmd/lit builds per run and every store open is handed, so the
	// ceiling is passed in at the composition root rather than read as a
	// global below it. Of leaves it 0, which moves no existing workspace's
	// schema: an Env nobody gave a ceiling fails closed.
	releasedSchema version.SchemaCeiling
	// raisePolicy is whether this binary may raise a remote's schema, as
	// cmd/lit built it from its build stamp (version.RaisePolicyOf). It rides
	// here for the reason releasedSchema does, and Of leaves it the zero
	// policy, which raises no remote: an Env nobody gave a policy fails closed.
	raisePolicy version.RaisePolicy
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

// UserCacheDir is the user's cache directory, resolved as os.UserCacheDir
// resolves it on this GOOS.
func (e Env) UserCacheDir() (string, error) {
	switch e.goos {
	case "windows":
		if dir := e.Get("LocalAppData"); dir != "" {
			return dir, nil
		}
		return "", errors.New("%LocalAppData% is not defined")
	case "darwin", "ios":
		home, err := e.HomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Caches"), nil
	case "plan9":
		home, err := e.HomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "lib", "cache"), nil
	}
	if dir := e.Get("XDG_CACHE_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New("path in $XDG_CACHE_HOME is relative")
		}
		return dir, nil
	}
	home, err := e.HomeDir()
	if err != nil {
		return "", errors.New("neither $XDG_CACHE_HOME nor $HOME are defined")
	}
	return filepath.Join(home, ".cache"), nil
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
	// The receiver is copied whole, so every field but vars rides along
	// without being named: a field added to Env cannot be dropped here.
	e.vars = kept
	return e
}

// With is the environment with the KEY=VALUE entries set appended, so each
// overrides any earlier entry for its key.
func (e Env) With(set ...string) Env {
	e.vars = append(slices.Clone(e.vars), set...)
	return e
}

// WithReleasedSchema is the environment with its released-schema ceiling set
// to ceiling: no existing workspace is migrated past it, and no commit above
// it is pushed.
func (e Env) WithReleasedSchema(ceiling version.SchemaCeiling) Env {
	e.releasedSchema = ceiling
	return e
}

// ReleasedSchema is the released-schema ceiling WithReleasedSchema set; its
// Max is 0 when none was.
func (e Env) ReleasedSchema() version.SchemaCeiling {
	return e.releasedSchema
}

// WithRaisePolicy is the environment with its raise policy set to policy: a
// push that would raise a remote's schema lands only as the policy permits.
func (e Env) WithRaisePolicy(policy version.RaisePolicy) Env {
	e.raisePolicy = policy
	return e
}

// RaisePolicy is the raise policy WithRaisePolicy set; the zero policy, which
// raises nothing, when none was.
func (e Env) RaisePolicy() version.RaisePolicy {
	return e.raisePolicy
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
// counted (GitSpawns), whether or not the git then starts; Dolt's own git,
// inside a fetch or push, is started by Dolt and is not.
// [LAW:nothing-unseen] [LAW:single-enforcer]
func (e Env) Git(ctx context.Context, dir string, stdin io.Reader, args ...string) *gitcmd.Cmd {
	e.countGit()
	return gitcmd.Command(ctx, gitcmd.Spec{Dir: dir, Env: e.in(dir), Stdin: stdin}, args...)
}

// countGit counts one git for the lineage. The zero Env is no run's
// environment and has no lineage to count for, so its git is counted nowhere;
// it still runs, in the empty environment the zero Env reads as.
func (e Env) countGit() {
	if e.IsZero() {
		return
	}
	e.gitSpawns.Add(1)
}

// GitSpawns is how many git commands Git has built for this Env's lineage —
// every git lit itself ran. The zero Env counts none.
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
