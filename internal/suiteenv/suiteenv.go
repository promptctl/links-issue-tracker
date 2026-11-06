// Package suiteenv builds the environment every test suite runs lit under.
// Only _test.go files import it.
package suiteenv

import (
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// Of is entries as an environ.Env whose released-schema ceiling is this tree's
// own highest migration, so a suite exercises every migration the tree
// carries, and whose raise policy is a release's, so a suite's stores raise as
// a released lit's would. Every field a suite's Env needs is added here, once.
func Of(entries []string) environ.Env {
	max, err := migrations.MaxVersion()
	if err != nil {
		panic("read the migration registry: " + err.Error())
	}
	return environ.Of(entries).
		WithReleasedSchema(version.SchemaCeiling{Max: max, Source: "the test suite runs every migration this tree carries"}).
		WithRaisePolicy(version.RaisePolicy{Release: true})
}
