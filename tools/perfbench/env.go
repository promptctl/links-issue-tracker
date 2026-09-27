package main

import "strings"

// hermeticEnv is the environment lit runs under while it is being measured:
// the caller's, with every setting that would make lit take a different code
// path on a different machine removed.
//
// WHAT IS REMOVED, AND WHY EACH ONE. lit layers a global config file under
// every command (internal/config: $XDG_CONFIG_HOME/links-issue-tracker, or
// LIT_CONFIG_GLOBAL_PATH), and that file can turn the inline receive off, so a
// developer with sync.receive=false would time every read probe without the
// debounce and remote check every other machine pays — two machines reporting
// different numbers for a code-path difference, with nothing in the table to
// say so. The same directory holds ejected templates, which is what
// `quickstart`, a control, renders. LIT_DISABLE_AUTO_SYNC skips the post-command
// maintenance outright, and CLAUDE_CODE_SESSION_ID changes what the write probe
// records as its assignee. So: XDG_CONFIG_HOME is pointed at an empty directory
// the run owns, and every LIT_* variable and the session id are dropped.
// [LAW:one-source-of-truth] the generated store's own .lit/config.toml is then
// the only configuration in force, at every size, on every machine.
//
// Everything else is kept, PATH and the cgo library paths included: the point
// is a lit that runs the same way everywhere, not a lit that cannot run.
//
// Pure — a function of the environment it is handed — so what it strips is
// testable without a process. [LAW:effects-at-boundaries]
func hermeticEnv(environ []string, configHome string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if key == "XDG_CONFIG_HOME" || key == "CLAUDE_CODE_SESSION_ID" || strings.HasPrefix(key, "LIT_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "XDG_CONFIG_HOME="+configHome)
}
