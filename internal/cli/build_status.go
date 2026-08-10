package cli

import (
	"fmt"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// buildStatusNote renders a short, always-present fragment naming whether this
// binary is a dev or release build, and — for a dev build with a known build
// date — how old it is. `lit doctor` and the sync/init decision points name
// this because a stale dev build silently missing a landed fix was the
// suspected root cause of the field incident the links-sync-pgct epic exists
// to prevent: knowing "this decision was made by a dev build" turns a
// mysterious failure into a five-minute diagnosis instead of a multi-step
// forensic reconstruction. [LAW:one-source-of-truth] version.Info (and its
// BuildAge method) is the only data source; this never re-derives IsDev or
// re-parses Date.
func buildStatusNote(info version.Info, now time.Time) string {
	if !info.IsDev {
		return fmt.Sprintf("build: release %s", info.Version)
	}
	age, ok := info.BuildAge(now)
	if !ok {
		return "build: dev build (build date unknown)"
	}
	if age >= version.StaleBuildThreshold {
		// "at least", not "older than": the guard is >=, so age can equal the
		// threshold exactly, and "built 7 days ago — older than 7 days" would
		// contradict itself at that exact boundary.
		return fmt.Sprintf(
			"build: dev build, built %s ago — STALE (at least %s old; run `just build` to refresh)",
			humanizeCoarseDuration(age), humanizeCoarseDuration(version.StaleBuildThreshold),
		)
	}
	return fmt.Sprintf("build: dev build, built %s ago", humanizeCoarseDuration(age))
}

// resolveBuildStatusNote resolves this binary's version.Info and renders it via
// buildStatusNote. Like sync freshness (resolveDoctorSyncFreshness), this is
// best-effort: a failure to resolve Info (the embedded migration registry
// could not be read) becomes a loud "status unavailable" fragment rather than
// aborting the caller — build status is a diagnostic value, never a
// correctness gate. [LAW:no-silent-failure] [LAW:effects-at-boundaries] the
// version lookup happens here, at the boundary, so callers render from one
// already-resolved value.
func resolveBuildStatusNote(now time.Time) string {
	info, err := version.Get()
	if err != nil {
		return fmt.Sprintf("build: status unavailable (%v)", err)
	}
	return buildStatusNote(info, now)
}
