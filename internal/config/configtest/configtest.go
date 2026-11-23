// Package configtest holds the one fixture through which a test in any
// package gets the project config it runs under.
package configtest

import "github.com/promptctl/links-issue-tracker/internal/config"

// Project is the project config layer whose content is toml, held as a value:
// no file is written or read, and no store or git repository is asked.
// Empty toml is a project that configures nothing, so the built-in defaults
// (under the global layer) are in force.
//
// [LAW:one-source-of-truth] every in-process test reaches a project config
// through this function, so a change to how the project config reaches a
// command is an edit here and in no test file. A test whose subject is the
// way a checkout's owner puts a config in force goes through the CLI's
// end-to-end helper instead.
func Project(toml string) config.ProjectLayer {
	return config.HandedProject([]byte(toml))
}
