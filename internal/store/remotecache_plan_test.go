package store

import (
	"strings"
	"testing"
)

const (
	liveKey   = "0a69e895025c221a9ea7a8570fba375f7f42b82be0f8478d65714c6f1c854737"
	orphanKey = "cccde2fa0f53c5811038500d340d66284c897b14b164ae2c59864637af8ed562"
	// missedKey is what remoteCacheKey produces for the live remote when the
	// `/./` is dropped from the URL — a plausible key naming no directory.
	missedKey = "f85e620f0560cadd1090ffeab0dd90dc06613c0a6024b5a6a740f9667f325ee3"
)

// TestPlanRemoteCachePruneCollectsOnlyUnmatchedDirs is the ordinary case: the
// derivation accounted for the live remote, so the leftover is abandoned.
func TestPlanRemoteCachePruneCollectsOnlyUnmatchedDirs(t *testing.T) {
	t.Parallel()
	plan, err := planRemoteCachePrune(
		map[string]string{liveKey: "origin"},
		[]string{liveKey, orphanKey},
	)
	if err != nil {
		t.Fatalf("planRemoteCachePrune() error = %v, want the plan", err)
	}
	if len(plan.abandoned) != 1 || plan.abandoned[0] != orphanKey {
		t.Fatalf("abandoned = %v, want exactly [%s]", plan.abandoned, orphanKey)
	}
}

// TestPlanRemoteCachePruneDeclinesWhenDerivationMissesTheLiveMirror is the whole
// reason this code is shaped the way it is. It reproduces the real near-miss:
// the live remote's URL carries a home-relative `/./`, a derivation that drops
// it yields missedKey, and missedKey names no directory on disk. Under a plain
// set subtraction the live mirror would be "unmatched" and deleted, the next
// push would silently re-clone it, and the prune would churn the entire cache on
// every push while reporting success. The plan must refuse instead.
func TestPlanRemoteCachePruneDeclinesWhenDerivationMissesTheLiveMirror(t *testing.T) {
	t.Parallel()
	plan, err := planRemoteCachePrune(
		map[string]string{missedKey: "origin"}, // what a drifted derivation predicts
		[]string{liveKey, orphanKey},           // what is actually on disk
	)
	if err == nil {
		t.Fatalf("planRemoteCachePrune() returned a plan (%v), want a refusal — "+
			"this plan would delete the live mirror", plan.abandoned)
	}
	if len(plan.abandoned) != 0 {
		t.Fatalf("a refused plan carried %v, want nothing deletable", plan.abandoned)
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Fatalf("refusal = %q, want it to name the remote it could not account for", err)
	}
}

// TestPlanRemoteCachePruneAcceptsAStoreThatHasNeverPushed proves the refusal does
// not fire on a fresh store. Nothing is on disk, so the unaccounted remote is
// just a cache not built yet and there is nothing to delete either way.
func TestPlanRemoteCachePruneAcceptsAStoreThatHasNeverPushed(t *testing.T) {
	t.Parallel()
	plan, err := planRemoteCachePrune(map[string]string{liveKey: "origin"}, nil)
	if err != nil {
		t.Fatalf("planRemoteCachePrune() error = %v, want a no-op plan", err)
	}
	if len(plan.abandoned) != 0 {
		t.Fatalf("abandoned = %v, want nothing", plan.abandoned)
	}
}

// TestPlanRemoteCachePruneCollectsWhenNoRemoteIsConfigured covers the store whose
// remotes were all removed: every mirror is abandoned, and nothing is
// unaccounted for, so collecting them is safe.
func TestPlanRemoteCachePruneCollectsWhenNoRemoteIsConfigured(t *testing.T) {
	t.Parallel()
	plan, err := planRemoteCachePrune(nil, []string{liveKey, orphanKey})
	if err != nil {
		t.Fatalf("planRemoteCachePrune() error = %v, want the plan", err)
	}
	if len(plan.abandoned) != 2 {
		t.Fatalf("abandoned = %v, want both mirrors", plan.abandoned)
	}
}

// TestRemoteCacheKeyPreservesHomeRelativePath pins the derivation against the
// value Dolt actually produced for this repository's own remote — the URL whose
// `/./` segment is the difference between naming the live 97 MB mirror and
// naming nothing at all.
func TestRemoteCacheKeyPreservesHomeRelativePath(t *testing.T) {
	t.Parallel()
	key, gitBacked, err := remoteCacheKey("git+ssh://git@github.com/./promptctl/links-issue-tracker.git")
	if err != nil {
		t.Fatalf("remoteCacheKey() error = %v", err)
	}
	if !gitBacked {
		t.Fatalf("gitBacked = false, want true for a git+ssh url")
	}
	if key != liveKey {
		t.Fatalf("key = %q, want %q — the home-relative `/./` must survive into the hash", key, liveKey)
	}
}

// TestRemoteCacheKeySeparatesNonGitRemotesFromBadUrls pins the three-way split.
// Folding the middle case either way is the enumeration gap: as an error, one
// ordinary file remote disables the prune; as a key, the prune invents a
// directory Dolt never wrote.
func TestRemoteCacheKeySeparatesNonGitRemotesFromBadUrls(t *testing.T) {
	t.Parallel()
	key, gitBacked, err := remoteCacheKey("file:///srv/backups/lit")
	if err != nil {
		t.Fatalf("a non-git remote is not an error, got %v", err)
	}
	if gitBacked || key != "" {
		t.Fatalf("file remote reported gitBacked=%v key=%q, want false and empty", gitBacked, key)
	}

	if _, _, err := remoteCacheKey("git+ssh://git@github.com/x\x7f.git"); err == nil {
		t.Fatalf("an unparseable url must be an error, not a silent skip")
	}
}

// TestIsRemoteCacheKeyRejectsForeignNames keeps the prune off anything dbfactory
// did not write. Every rejected name here would otherwise be classified
// abandoned and deleted.
func TestIsRemoteCacheKeyRejectsForeignNames(t *testing.T) {
	t.Parallel()
	if !isRemoteCacheKey(liveKey) {
		t.Fatalf("isRemoteCacheKey(%q) = false, want true", liveKey)
	}
	for _, name := range []string{
		"",
		"repo.git",
		"init.lock",
		strings.ToUpper(liveKey), // dbfactory writes lowercase hex
		liveKey[:63],             // too short
		liveKey + "0",            // too long
		strings.Repeat("z", 64),  // right length, not hex
		"../" + liveKey[:61],     // traversal-shaped
	} {
		if isRemoteCacheKey(name) {
			t.Fatalf("isRemoteCacheKey(%q) = true, want false", name)
		}
	}
}

// TestPruneOutcomeReportKeepsConfirmedWorkAfterAProblem pins the partial-progress
// case. Removing a directory is not undone by a later error, so a run that
// collected mirrors and then failed must report both facts — reporting only the
// failure destroys the record of real disk that was really reclaimed, at the one
// surface that reports it.
func TestPruneOutcomeReportKeepsConfirmedWorkAfterAProblem(t *testing.T) {
	t.Parallel()
	got := remoteCachePruneOutcome{Removed: 2, Reclaimed: 3 << 20, Problem: "remove abandoned mirror abc: permission denied"}.Report()
	for _, want := range []string{"removed 2", "3.0 MiB", "then failed", "permission denied"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Report() = %q, want it to contain %q", got, want)
		}
	}
}

// TestPruneOutcomeReportIsEmptyOnlyWhenNothingHappened pins the silence
// contract the CLI depends on: every state a reader would act on renders
// non-empty, so an empty report can never hide one.
func TestPruneOutcomeReportIsEmptyOnlyWhenNothingHappened(t *testing.T) {
	t.Parallel()
	if got := (remoteCachePruneOutcome{}).Report(); got != "" {
		t.Fatalf("Report() on a no-op = %q, want empty", got)
	}
	for name, outcome := range map[string]remoteCachePruneOutcome{
		"work performed": {Removed: 1, Reclaimed: 4096},
		"declined":       {Problem: "declining to prune: ..."},
		"io failure":     {Problem: "remove abandoned mirror abc: permission denied"},
		"partial":        {Removed: 1, Reclaimed: 4096, Problem: "boom"},
	} {
		if outcome.Report() == "" {
			t.Fatalf("Report() for %q was empty — an actionable state must never render silent", name)
		}
	}
}
