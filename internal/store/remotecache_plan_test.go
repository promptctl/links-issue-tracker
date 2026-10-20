package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
)

const (
	liveKey   = "0a69e895025c221a9ea7a8570fba375f7f42b82be0f8478d65714c6f1c854737"
	orphanKey = "cccde2fa0f53c5811038500d340d66284c897b14b164ae2c59864637af8ed562"
	// unopenedKey is a configured remote's key with no directory on disk: a
	// remote this store has not opened yet.
	unopenedKey = "f85e620f0560cadd1090ffeab0dd90dc06613c0a6024b5a6a740f9667f325ee3"
)

// TestSplitMirrorsAbandonsOnlyWhatNoRemoteNames is the ordinary case: the
// directory a configured remote's key names is live, the leftover is
// abandoned, and a configured remote not yet opened here changes neither.
func TestSplitMirrorsAbandonsOnlyWhatNoRemoteNames(t *testing.T) {
	t.Parallel()
	live, abandoned := splitMirrors(
		map[string]string{"origin": liveKey, "backup": unopenedKey},
		[]string{orphanKey, liveKey},
	)
	if !slices.Equal(live, []string{liveKey}) || !slices.Equal(abandoned, []string{orphanKey}) {
		t.Fatalf("splitMirrors() = (live %v, abandoned %v), want ([%s], [%s])", live, abandoned, liveKey, orphanKey)
	}
}

// TestCollectAbandonedMirrorTreatsAnAlreadyGoneDirectoryAsDone pins the
// concurrent-prune case. Explicit pushes are not serialized against each other,
// so a sibling can collect a planned directory between the plan and this walk —
// which is the state this code was trying to reach. Calling that a failure would
// spend exactly the credibility the refusal message depends on.
func TestCollectAbandonedMirrorTreatsAnAlreadyGoneDirectoryAsDone(t *testing.T) {
	t.Parallel()
	reclaimed, collected, err := collectAbandonedMirror(t.TempDir(), orphanKey)
	if err != nil {
		t.Fatalf("collectAbandonedMirror() on a missing directory = %v, want no error — "+
			"someone else already did the work", err)
	}
	if collected || reclaimed != 0 {
		t.Fatalf("collectAbandonedMirror() = (%d, %v), want (0, false) — this call collected nothing",
			reclaimed, collected)
	}
}

// TestCollectAbandonedMirrorReclaimsWhatItRemoves is the ordinary case, and it
// checks the byte count against a known payload so the reclaim figure is a
// measurement rather than a directory tally.
func TestCollectAbandonedMirrorReclaimsWhatItRemoves(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repo := filepath.Join(base, orphanKey, "repo.git")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "packed-refs"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("plant orphan payload: %v", err)
	}

	reclaimed, collected, err := collectAbandonedMirror(base, orphanKey)
	if err != nil {
		t.Fatalf("collectAbandonedMirror() error = %v", err)
	}
	if !collected || reclaimed != 4096 {
		t.Fatalf("collectAbandonedMirror() = (%d, %v), want (4096, true)", reclaimed, collected)
	}
	if _, err := os.Stat(filepath.Join(base, orphanKey)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the mirror survived a successful collection: stat err = %v", err)
	}
}

// TestCollectAbandonedMirrorReportsAFailureNamingTheKey keeps a real failure
// loud and locatable. The outcome joins these, so an error that does not name
// its key leaves an operator with a count and nowhere to look.
func TestCollectAbandonedMirrorReportsAFailureNamingTheKey(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, orphanKey), 0o755); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	// Unlinking the key directory needs write on its parent, so a read-only base
	// fails the removal while leaving the walk that measures it working.
	fsperm.Deny(t, base, fsperm.Write)

	_, collected, err := collectAbandonedMirror(base, orphanKey)
	if err == nil {
		t.Fatal("collectAbandonedMirror() on an unremovable directory returned no error")
	}
	if collected {
		t.Fatal("collectAbandonedMirror() reported a collection it did not make")
	}
	if !strings.Contains(err.Error(), orphanKey) {
		t.Fatalf("error = %q, want it to name the key it failed on", err)
	}
}

// TestSplitMirrorsAbandonsEveryMirrorWhenNoRemoteIsConfigured covers the store
// whose remotes were all removed: no open can reach any mirror.
func TestSplitMirrorsAbandonsEveryMirrorWhenNoRemoteIsConfigured(t *testing.T) {
	t.Parallel()
	live, abandoned := splitMirrors(nil, []string{orphanKey, liveKey})
	if len(live) != 0 || !slices.Equal(abandoned, []string{liveKey, orphanKey}) {
		t.Fatalf("splitMirrors() = (live %v, abandoned %v), want (none, both, sorted)", live, abandoned)
	}
}

// TestGitRemoteCacheKeyPreservesHomeRelativePath pins the key against the value
// Dolt actually produced for this repository's own remote, the URL whose `/./`
// segment is the difference between naming the live 97 MB mirror and naming
// nothing at all. It holds the fork to that across rebases.
func TestGitRemoteCacheKeyPreservesHomeRelativePath(t *testing.T) {
	t.Parallel()
	key, gitBacked, err := dbfactory.GitRemoteCacheKey("git+ssh://git@github.com/./promptctl/links-issue-tracker.git", nil)
	if err != nil {
		t.Fatalf("GitRemoteCacheKey() error = %v", err)
	}
	if !gitBacked || key != liveKey {
		t.Fatalf("GitRemoteCacheKey() = (%q, %v), want (%q, true)", key, gitBacked, liveKey)
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
	got := remoteCachePruneOutcome{
		Removed: 2, Reclaimed: 3 << 20, Problem: "remove abandoned mirror abc: permission denied",
	}.Report()
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
	for name, collection := range map[string]remoteCacheCollection{
		"work performed":     {Removed: 1, Reclaimed: 4096},
		"re-check failed":    {Problem: "re-check abandoned mirror abc: list dolt remotes: ..."},
		"io failure":         {Problem: "remove abandoned mirror abc: permission denied"},
		"partial":            {Removed: 1, Reclaimed: 4096, Problem: "boom"},
		"finished by a peer": {Reclaimed: 4096},
	} {
		if remoteCachePruneOutcome(collection).Report() == "" {
			t.Fatalf("Report() for %q was empty — an actionable state must never render silent", name)
		}
	}
}

// TestCollectAbandonedMirrorKeepsCollectingPastAStuckFile pins two things. The
// reclaim figure counts unlinks, not a measurement, and a failure does not
// erase it. And a file that cannot be unlinked does not stop its siblings: the
// stuck one sorts first, so a walk that stopped at it would free nothing and
// pin the rest of the mirror on every prune that reaches it.
func TestCollectAbandonedMirrorKeepsCollectingPastAStuckFile(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	stuck, free := filepath.Join(base, orphanKey, "a-stuck"), filepath.Join(base, orphanKey, "b-free")
	for _, dir := range []string{free, stuck} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("plant orphan: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(free, "pack"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("plant free payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "pack"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("plant stuck payload: %v", err)
	}
	fsperm.Deny(t, stuck, fsperm.Write)

	reclaimed, collected, err := collectAbandonedMirror(base, orphanKey)
	if err == nil || collected {
		t.Fatalf("collectAbandonedMirror() = (collected %v, err %v), want a failure and no collection", collected, err)
	}
	if reclaimed != 4096 {
		t.Fatalf("reclaimed = %d, want 4096 — exactly the file it unlinked, not the one it could not", reclaimed)
	}
}

// TestCollectAbandonedMirrorsDeletesOnlyWhatTheFreshReadAbandons pins that the
// loop decides each deletion on a read taken just before it, not on the read it
// started from. Between the two, a key can come back to life.
func TestCollectAbandonedMirrorsDeletesOnlyWhatTheFreshReadAbandons(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	revived, abandoned := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	for _, key := range []string{revived, abandoned} {
		if err := os.MkdirAll(filepath.Join(base, key, "repo.git"), 0o755); err != nil {
			t.Fatalf("plant %s: %v", key, err)
		}
	}
	outcome := collectAbandonedMirrors(base, []string{revived, abandoned},
		func() ([]string, error) { return []string{abandoned}, nil })
	if outcome.Removed != 1 || outcome.Problem != "" {
		t.Fatalf("outcome = %+v, want exactly one removal and no problem", outcome)
	}
	if _, err := os.Stat(filepath.Join(base, revived)); err != nil {
		t.Fatalf("a key the fresh read no longer abandons was deleted: %v", err)
	}

	failed := errors.New("list dolt remotes: connection closed")
	outcome = collectAbandonedMirrors(base, []string{revived},
		func() ([]string, error) { return nil, failed })
	if outcome.Removed != 0 || !strings.Contains(outcome.Problem, "re-check abandoned mirror "+revived) ||
		!strings.Contains(outcome.Problem, failed.Error()) {
		t.Fatalf("outcome = %+v, want no removal and the failed read reported against %s", outcome, revived)
	}
	if _, err := os.Stat(filepath.Join(base, revived)); err != nil {
		t.Fatalf("a key was deleted after the fresh read failed: %v", err)
	}
}
