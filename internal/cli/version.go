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
	if age, ok := info.BuildAge(time.Now()); ok {
		if _, err := fmt.Fprintf(stdout, "built %s ago\n", humanizeDuration(age)); err != nil {
			return err
		}
		if age >= version.StaleBuildThreshold {
			if _, err := fmt.Fprintf(stdout,
				"WARNING: binary is older than %s — run `just build` (or `just install`) to pick up recent fixes\n",
				humanizeDuration(version.StaleBuildThreshold),
			); err != nil {
				return err
			}
		}
	}

	_, err = fmt.Fprintf(stdout, "schema versions supported: %d–%d\n", info.Schema.Min, info.Schema.Max)
	return err
}

// humanizeDuration renders a duration as a coarse, human-legible phrase — the
// same day/hour/minute bucketing convention SyncFailure.agePhrase
// (sync_failure.go) uses for divergence age, so "how stale" reads
// consistently across lit's surfaces.
func humanizeDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	default:
		return "under a minute"
	}
}
