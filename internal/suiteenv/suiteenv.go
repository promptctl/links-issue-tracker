// Package suiteenv builds the environment a test suite runs lit's stores
// under. Only _test.go files import it; depguard enforces that.
package suiteenv

import (
	"sync"

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
func Of(entries []string) environ.Env {
	max, err := maxVersion()
	if err != nil {
		panic("read the migration registry: " + err.Error())
	}
	return environ.Of(entries).
		WithReleasedSchema(version.SchemaCeiling{Max: max, Bound: version.BoundReleased, Source: "the test suite runs every migration this tree carries"}).
		WithRaisePolicy(version.RaisePolicy{Release: true})
}
