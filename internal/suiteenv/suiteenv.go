// Package suiteenv builds the environment a test suite runs lit's stores
// under. Only _test.go files import it; depguard enforces that.
package suiteenv

import (
	"sync"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// maxVersion is read once: the embedded registry is fixed for the binary's life.
var maxVersion = sync.OnceValues(migrations.MaxVersion)

// StallBound is the one deadline a suite's runs carry for every figure of
// deadline.Deadlines, in the test process and in every worker it spawns. The
// shipped figures are sized to a healthy run's real cost, so they sit close
// enough to the local work these tests really do — a clone, a push to a file
// remote, a receive that follows a re-root — that a busy machine reaches them,
// and a test with no interest in a deadline would take its verdict from the
// load. [LAW:no-ambient-temporal-coupling] A minute is still a stall detector:
// over ten times that work, and far below go test's timeout, so a hang ends
// inside the test that met it and the package's other verdicts stand. A test
// about a deadline hands its runs its own figure.
const StallBound = time.Minute

// Deadlines is the deadlines a suite's runs carry: every figure the stall
// bound. The cli and store suites both read it, through Of or directly, so
// the suites' deadline policy has this one home. [LAW:one-source-of-truth]
func Deadlines() deadline.Deadlines {
	d, err := deadline.Of(deadline.Figures{MirrorHold: StallBound, MirrorPush: StallBound, Receive: StallBound, PushProof: StallBound, Compaction: StallBound})
	if err != nil {
		panic("build the suite's deadlines: " + err.Error())
	}
	return d
}

// ContentionHold is the mirror hold a test whose subject is a busy refusal
// hands its runs: the smallest a test can hand, so the holder wait its runs
// derive (deadline.Deadlines.HolderWait) is the shortest a run can have, and
// the refusal arrives in about a second and a half rather than after the
// stall bound's minute.
const ContentionHold = 100 * time.Millisecond

// WithHold is the suite's deadlines with the mirror hold at hold, for a test
// whose subject is the holder wait or a refusal that waits it out: every open
// and lock wait its runs make derives its wait from the deadlines it is
// handed, so the test hands them, and no other test sees them.
// [LAW:no-shared-mutable-globals] A hold that is not positive panics, as
// Deadlines does: the figure is the test's own constant.
func WithHold(hold time.Duration) deadline.Deadlines {
	figures := Deadlines().Figures()
	figures.MirrorHold = hold
	d, err := deadline.Of(figures)
	if err != nil {
		panic("build the suite's deadlines with a hold of " + hold.String() + ": " + err.Error())
	}
	return d
}

// Of is entries as an environ.Env whose released-schema ceiling is this tree's
// own highest migration, so a suite exercises every migration the tree
// carries, whose raise policy is a release's, so a suite's stores raise as a
// released lit's would, and whose deadlines are the suite's (Deadlines). Every
// field a suite's Env needs is added here, once.
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
		WithRaisePolicy(version.RaisePolicy{Release: true}).
		WithDeadlines(Deadlines())
}
