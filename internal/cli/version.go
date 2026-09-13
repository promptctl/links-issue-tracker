package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// runVersion is the user-facing surface for the binary's identity: it prints
// the version, commit, build date, build age, and supported schema range from
// version.Info.
//
// [LAW:one-source-of-truth] version.Info (and its BuildAge method) is the
// only data source.
func runVersion(stdout io.Writer, args []string) error {
	fs := newCobraFlagSet("version")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit version"}
	}

	info, err := version.Get()
	if err != nil {
		return err
	}

	ver := info.Version
	if info.IsDev {
		ver = "dev"
	}
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	date := info.Date
	if date == "" {
		date = "unknown"
	}
	if _, err := fmt.Fprintf(stdout, "lit %s (commit %s, built %s)\n", ver, commit, date); err != nil {
		return err
	}

	// Age reporting only fires when Date parsed to a real, past instant — a
	// build with no Date (pre-ldflags `go build`, or a corrupted stamp) prints
	// no age line rather than a fabricated one. [LAW:no-defensive-null-guards]
	// this is a trust-boundary check on link-time-injected input, not a guard
	// papering over a value that should never be absent.
	now := time.Now()
	if age, ok := info.BuildAge(now); ok {
		if _, err := fmt.Fprintf(stdout, "built %s ago\n", humanizeCoarseDuration(age)); err != nil {
			return err
		}
	}
	for _, line := range versionStalenessWarning(info, now) {
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintf(stdout, "schema versions supported: %d–%d\n", info.Schema.Min, info.Schema.Max)
	return err
}

// versionStalenessWarning renders `lit version`'s own staleness warning: zero
// or one line, in the shape buildStalenessLines uses, so this surface is a
// pure function of (info, now) rather than words trapped inside an io.Writer
// call. TestEveryStalenessSurfaceAgreesAtItsBoundary holds it as a row beside
// its three siblings — the point of extracting it, since the wording is only
// falsifiable at exactly the threshold and runVersion cannot be handed a
// synthetic Info. [LAW:effects-at-boundaries]
//
// The verdict comes from version.StaleSourceBuild, not from a second
// comparison against StaleBuildThreshold here. This surface used to warn on
// age alone, which told the holder of a months-old *release* binary to run
// `just build` — advice that does not refresh it — while the build-status
// note on doctor/sync/init reached the opposite verdict about that same
// binary. One predicate now answers for both. [LAW:single-enforcer]
//
// Both halves of the sentence are borrowed, not retyped: the threshold
// parenthetical from stalenessThresholdClause and the cure from
// buildRefreshRemedy. This line said "older than 7 days" while gating on `>=`,
// which is false for the binary built exactly 7 days ago — the first one the
// gate speaks about. [LAW:one-source-of-truth]
func versionStalenessWarning(info version.Info, now time.Time) []string {
	if _, stale := info.StaleSourceBuild(now); !stale {
		return nil
	}
	return []string{fmt.Sprintf(
		"WARNING: this build is %s — %s",
		stalenessThresholdClause(version.StaleBuildThreshold), buildRefreshRemedy,
	)}
}
