// Package builtcli runs a CLI its test binary's setup builds apart from
// itself, the way a suite whose TestMain builds the real program does: the
// setup never runs the CLI, and the program is a path known only at run time.
package builtcli

import "os/exec"

// Run runs the program at path.
func Run(path string) error { return exec.Command(path, "version").Run() }
