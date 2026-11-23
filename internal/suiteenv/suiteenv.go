// Package suiteenv builds the environment a test suite runs lit's stores
// under. Only _test.go files import it; depguard enforces that.
package suiteenv

import (
	"os"
	"sync"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// maxVersion is read once: the embedded registry is fixed for the binary's life.
var maxVersion = sync.OnceValues(migrations.MaxVersion)

// Of is entries as an environ.Env whose released-schema ceiling is this tree's
// own highest migration, so a suite exercises every migration the tree
// carries, and whose raise policy is a release's, so a suite's stores raise as
// a released lit's would. Every field a suite's Env needs is added here, once.
// The config path overrides are left out, so no test runs under a config file
// the developer's shell points at; a test of an override sets it itself.
func Of(entries []string) environ.Env {
	max, err := maxVersion()
	if err != nil {
		panic("read the migration registry: " + err.Error())
	}
	return environ.Of(entries).
		Without(config.PathEnvVars()...).
		WithReleasedSchema(version.SchemaCeiling{Max: max, Bound: version.BoundReleased, Source: "the test suite runs every migration this tree carries"}).
		WithRaisePolicy(version.RaisePolicy{Release: true})
}

// UnsetConfigPaths removes the config path overrides from the process
// environment, for a TestMain whose tests start children that inherit it, so
// those children read no config file the developer's shell points at either.
func UnsetConfigPaths() {
	for _, key := range config.PathEnvVars() {
		if err := os.Unsetenv(key); err != nil {
			panic("unset " + key + ": " + err.Error())
		}
	}
}
