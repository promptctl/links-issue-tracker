package cli

import (
	"fmt"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// buildStatusNote and buildStalenessLines gate on the same
// version.Info.StaleSourceBuild predicate, so they speak about the same
// binaries at the same ages and cannot prescribe different cures.
// buildRefreshRemedy is the one they both name. The predicate covers both
// from-source shapes, so the remedy has to as well: `just build` refreshes the
// ./lit a developer runs out of the repo, `just install` the one on PATH.
// Naming either alone tells half the population to rebuild a binary they are
// not running — the banner named only `just install`, which does not refresh
// the ./lit in front of the reader. [LAW:one-source-of-truth]
//
// The other half of the shared vocabulary — how the threshold itself is
// phrased — belongs to stalenessThresholdClause, which every staleness surface
// calls and which owns the reason it reads "at least".
const buildRefreshRemedy = "run `just build` (or `just install`) to refresh"

// buildStatusNote renders a short, always-present fragment naming whether this
// binary is a dev or release build, and — for a dev build with a known build
// date — how old it is. `lit doctor` and the sync/init decision points name
// this because a stale dev build silently missing a landed fix was the
// suspected root cause of the field incident the links-sync-pgct epic exists
// to prevent: knowing "this decision was made by a dev build" turns a
// mysterious failure into a five-minute diagnosis instead of a multi-step
// forensic reconstruction. [LAW:one-source-of-truth] version.Info (and its
// BuildAge and StaleSourceBuild methods) is the only data source; this never
// re-derives provenance or re-parses Date.
//
// Keyed on FromSource, not IsDev. IsDev asks whether a Version was stamped, and
// `just install` stamps one from `git describe` — so every binary this repo
// installs onto a PATH used to render as "build: release 0.14.0-21-g…" with its
// age unmentioned, while being exactly the locally-built binary whose age is
// the whole point of the note.
func buildStatusNote(info version.Info, now time.Time) string {
	if !info.FromSource {
		return fmt.Sprintf("build: release %s", info.Version)
	}
	age, ok := info.BuildAge(now)
	if !ok {
		return "build: dev build (build date unknown)"
	}
	if _, stale := info.StaleSourceBuild(now); stale {
		return fmt.Sprintf(
			"build: dev build, built %s ago — STALE (%s; %s)",
			humanizeCoarseDuration(age), stalenessThresholdClause(version.StaleBuildThreshold),
			buildRefreshRemedy,
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

// buildStalenessLines renders the rare, loud warning that the answer about to
// be printed is being produced by a binary its own working tree has moved past —
// the links-build-status-1svs surface. Zero or one line, in the shape and voice
// syncStalenessLines uses, because build drift is drift of the same kind as an
// unpushed commit or an unfetched remote and earns the same position: first on
// screen, on the commands an agent actually runs, rather than in a diagnostic
// nobody runs unasked.
//
// Rarity is the contract, not a nicety: a note that prints on every invocation
// gets tuned out and takes the loud case with it. version.Info's accept/reject
// table is what keeps it rare — a release build, a fresh source build, and a
// source build with no trustworthy Date are all silent, which is every ordinary
// invocation, leaving only the binary that has actually fallen behind. Pure
// over its inputs so that table is testable with no live store, mirroring
// syncStalenessLines' split from its own resolve step.
// [LAW:dataflow-not-control-flow]
func buildStalenessLines(info version.Info, now time.Time) []string {
	age, stale := info.StaleSourceBuild(now)
	if !stale {
		return nil
	}
	// One banner serves three call sites — `lit next`, `lit backlog` and the
	// full-detail `lit show` — so every claim it makes has to hold at all
	// three. It named "the routing behind this answer" until review caught
	// that `lit show <id>` routes nothing: the caller names the ticket, so the
	// line described two of its sites and invented a concern on the third. It
	// names the binary and the answer, never the work behind the answer.
	// [LAW:one-source-of-truth] one claim, one meaning, at every site it reaches.
	return []string{fmt.Sprintf(
		"build: this binary was built %s ago (%s) — the answer below may predate fixes already on master; %s",
		humanizeCoarseDuration(age), stalenessThresholdClause(version.StaleBuildThreshold),
		buildRefreshRemedy,
	)}
}

// resolveBuildStalenessLines resolves this binary's version.Info and renders it
// via buildStalenessLines — the one-call ergonomic resolveBuildStatusNote gives
// the always-present note. A failure to resolve Info is not silence here: it
// means the embedded migration registry could not be read, which is a binary
// unable to account for itself at all — strictly worse news than the stale
// binary this banner exists to announce, and so announced rather than
// swallowed. It stays a banner line rather than an error because this surface
// is supplementary: a read command must still answer.
// [LAW:no-silent-failure] [LAW:effects-at-boundaries] the version lookup
// happens here, at the boundary, so the renderer above stays pure.
func resolveBuildStalenessLines(now time.Time) []string {
	info, err := version.Get()
	if err != nil {
		return []string{fmt.Sprintf(
			"build: this binary cannot report its own identity (%v) — its age and provenance are unknown",
			err,
		)}
	}
	return buildStalenessLines(info, now)
}
