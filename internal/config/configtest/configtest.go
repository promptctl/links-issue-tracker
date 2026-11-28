// Package configtest holds the one fixture through which a test in any
// package gets the project config it runs under.
package configtest

import (
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Applied is the project config layer of a store toml was applied to, as a
// value: no file is written or read, and no store or git repository is asked.
// Empty toml is a project that configures nothing, so the built-in defaults
// (under the global layer) are in force.
//
// [LAW:one-source-of-truth] a test that holds its config as a value reaches
// it through this function, so a change to how the project config reaches a
// command is an edit here and in no test file. A test in which lit resolves a
// checkout from disk applies that checkout's file through the CLI's
// end-to-end helper instead. The config package's own tests build layers with
// its constructors, since it cannot import this one.
func Applied(toml string) config.ProjectLayer {
	return config.Applied([]model.ConfigApply{{ID: "cfg-configtest", Content: toml}})
}
