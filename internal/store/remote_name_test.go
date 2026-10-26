package store

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func remoteName(t *testing.T, raw string) storage.RemoteName {
	t.Helper()
	name, err := storage.ParseRemoteName(raw)
	if err != nil {
		t.Fatalf("storage.ParseRemoteName(%q): %v", raw, err)
	}
	return name
}

// TestRemoteNameCharactersAreDolts: storage's copy of the characters a
// remote's name may not hold is Dolt's own, character for character.
func TestRemoteNameCharactersAreDolts(t *testing.T) {
	t.Parallel()
	if storage.InvalidRemoteNameCharacters != doltenv.InvalidRemoteNameCharacters {
		t.Fatalf("storage.InvalidRemoteNameCharacters = %q, want Dolt's %q", storage.InvalidRemoteNameCharacters, doltenv.InvalidRemoteNameCharacters)
	}
}

// TestRefOwnerIsDoltsRemote: the owner lit reads off a held name is the
// remote Dolt reads off the tracking refs that name's fetches write.
func TestRefOwnerIsDoltsRemote(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"origin", "a/b", "team/origin", "a/b/c", "a//b"} {
		owner := ownerOfHeld(name)
		tracking, err := ref.NewRemoteRefFromPathStr("remotes/" + name + "/master")
		if err != nil {
			t.Fatalf("Dolt's parse of %s's tracking ref: %v", name, err)
		}
		if remote := tracking.(ref.RemoteRef).GetRemote(); owner.String() != remote {
			t.Errorf("ownerOfHeld(%q) = %q, want Dolt's remote %q", name, owner, remote)
		}
	}
}

// TestParseRemoteNameAdmitsWhatDoltHolds: DOLT_REMOTE add takes every name
// the parse admits and refuses every name it refuses. DOLT_CLONE --remote
// checks no name, which is why AdoptRemoteByClone takes a RemoteName.
func TestParseRemoteNameAdmitsWhatDoltHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	url := "file://" + filepath.Join(t.TempDir(), "remote")
	for _, name := range []string{"origin", "fork-2_old", "a/b", "upstream.old", "my fork"} {
		_, parseErr := storage.ParseRemoteName(name)
		_, doltErr := callIntProcedure(ctx, st.db, "DOLT_REMOTE", "add", name, url)
		if (parseErr == nil) != (doltErr == nil) {
			t.Errorf("%q: ParseRemoteName error = %v, Dolt's add error = %v; want both or neither", name, parseErr, doltErr)
		}
	}
}

// fetchedRemote adds name through the store and fetches it twice, so the
// second fetch records the fork point of every branch the first one brought.
// It returns the tracking refs' heads, keyed by branch.
func fetchedRemote(t *testing.T, ctx context.Context, st *Store, name, url string) map[string]string {
	t.Helper()
	if err := st.SyncAddRemote(ctx, remoteName(t, name), url); err != nil {
		t.Fatalf("SyncAddRemote(%s): %v", name, err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, name)); err != nil {
		t.Fatalf("SyncFetch(%s): %v", name, err)
	}
	if err := st.SyncResetToRemoteHead(ctx, remoteName(t, name), "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead(%s): %v", name, err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, name)); err != nil {
		t.Fatalf("SyncFetch(%s) again: %v", name, err)
	}
	heads := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/"+name+"/")
	if forkPoints := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+name+"/"); len(heads) == 0 || !maps.Equal(forkPoints, heads) {
		t.Fatalf("%s's fork points = %v, want its tracking refs' heads %v", name, forkPoints, heads)
	}
	return heads
}

func headsUnder(t *testing.T, ctx context.Context, st *Store, table, prefix string) map[string]string {
	t.Helper()
	heads, err := refHeadsUnder(ctx, st.db, `SELECT name, hash FROM `+table, prefix)
	if err != nil {
		t.Fatalf("list %s under %s: %v", table, prefix, err)
	}
	return heads
}

// TestRemovingARemoteTakesOnlyItsOwnRefs: removing a drops a's tracking refs
// and fork points and leaves c's as they were.
func TestRemovingARemoteTakesOnlyItsOwnRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := "file://" + filepath.Join(t.TempDir(), "remote")
	seedReconcileRemote(t, ctx, migratedDoltDir(t), url)
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	fetchedRemote(t, ctx, st, "a", url)
	cHeads := fetchedRemote(t, ctx, st, "c", url)

	aHeads := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/")
	if err := st.SyncRemoveRemote(ctx, "a"); err != nil {
		t.Fatalf("SyncRemoveRemote(a): %v", err)
	}
	want := []storage.RemoteRemovalRecord{{Remote: "a", Owner: "a", Held: true, TrackingRefsDropped: len(aHeads)}}
	if removals := st.RemoteRemovals(); !slices.Equal(removals, want) {
		t.Errorf("RemoteRemovals() = %+v, want %+v", removals, want)
	}
	if left := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/"); len(left) != 0 {
		t.Errorf("tracking refs under remotes/a/ = %v, want none", left)
	}
	if left := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"a/"); len(left) != 0 {
		t.Errorf("fork points under lit-fork-point/a/ = %v, want none", left)
	}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/c/"); !maps.Equal(tracked, cHeads) {
		t.Errorf("c's tracking refs = %v, want %v", tracked, cHeads)
	}
	if forkPoints := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"c/"); !maps.Equal(forkPoints, cHeads) {
		t.Errorf("c's fork points = %v, want %v", forkPoints, cHeads)
	}
}

// cloneHeldRemote is a checkout an older lit adopted by clone from url under
// the git remote name, which DOLT_CLONE --remote took unchecked, with the
// fork point of each tracking ref the clone wrote at its head. It returns the
// store and those heads, keyed by tracking-ref path under remotes/.
func cloneHeldRemote(t *testing.T, ctx context.Context, name, url string) (*Store, map[string]string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create the clone's root: %v", err)
	}
	db, err := openDoltPool(root, "ws", "", newLockHold(context.Background(), root, engineWrite))
	if err != nil {
		t.Fatalf("open dolt for the clone: %v", err)
	}
	if _, err := callIntProcedure(ctx, db, "DOLT_CLONE", "--remote", name, "--branch", "master", url, doltDatabaseName); err != nil {
		t.Fatalf("clone %s under remote %q: %v", url, name, err)
	}
	// Closed as cloneRemoteDatabase closes it: the embedded engine's close
	// reports the canceled context of the clone it served.
	_ = db.Close()
	st := openSyncOrFatal(t, ctx, root)
	t.Cleanup(func() { _ = st.Close() })
	heads := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/")
	if len(heads) == 0 {
		t.Fatalf("the clone under %q wrote no tracking ref", name)
	}
	for path, head := range heads {
		if err := execProcedureDiscard(ctx, st.db, "DOLT_BRANCH", "-f", forkPointPrefix+path, head); err != nil {
			t.Fatalf("record the fork point of %s: %v", path, err)
		}
	}
	return st, heads
}

// TestRemovingACloneHeldSlashNamedRemoteTakesItsRefs: a checkout an older lit
// adopted from team/origin holds that name. Its refs sit under team, and no
// remote named team remains, so every ref under remotes/team/ and
// lit-fork-point/team/ goes with it. A remote team added after, as git renaming
// team/origin to team asks, holds exactly its own server's branches.
func TestRemovingACloneHeldSlashNamedRemoteTakesItsRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := seededRemoteURL(t, ctx)
	st, heads := cloneHeldRemote(t, ctx, "team/origin", url)

	if err := st.SyncRemoveRemote(ctx, "team/origin"); err != nil {
		t.Fatalf("SyncRemoveRemote(team/origin): %v", err)
	}
	if left := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/team/"); len(left) != 0 {
		t.Errorf("tracking refs under remotes/team/ = %v, want none", left)
	}
	if left := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"team/"); len(left) != 0 {
		t.Errorf("fork points under lit-fork-point/team/ = %v, want none", left)
	}
	want := []storage.ForkPointRecord{{Remote: "team", Branch: "origin/master", Head: heads["team/origin/master"], Outcome: storage.ForkPointDropped}}
	if records := st.ForkPointRecords(); !slices.Equal(records, want) {
		t.Errorf("ForkPointRecords() = %+v, want %+v", records, want)
	}
	// A second reconcile planned before the first removal landed finds it
	// done, and removes nothing more.
	if err := st.SyncRemoveRemote(ctx, "team/origin"); err != nil {
		t.Fatalf("SyncRemoveRemote(team/origin) again: %v", err)
	}
	wantRemovals := []storage.RemoteRemovalRecord{
		{Remote: "team/origin", Owner: "team", Held: true, TrackingRefsDropped: len(heads)},
		{Remote: "team/origin", Owner: "team"},
	}
	if removals := st.RemoteRemovals(); !slices.Equal(removals, wantRemovals) {
		t.Errorf("RemoteRemovals() = %+v, want %+v", removals, wantRemovals)
	}

	if err := st.SyncAddRemote(ctx, remoteName(t, "team"), url); err != nil {
		t.Fatalf("SyncAddRemote(team): %v", err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, "team")); err != nil {
		t.Fatalf("SyncFetch(team): %v", err)
	}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/team/"); len(tracked) != 1 || tracked["master"] != heads["team/origin/master"] {
		t.Errorf("tracking refs under remotes/team/ after team's fetch = %v, want only master at %s", tracked, heads["team/origin/master"])
	}
}

// TestRemovingACloneHeldSlashNamedRemoteLeavesItsOwnersRefs: a/b's refs are
// a's refs for branches b/*, so removing a clone-held a/b beside a drops none
// of them, and a's next fetch, an ordinary one, prunes them and their fork
// points and leaves exactly a's own branches.
func TestRemovingACloneHeldSlashNamedRemoteLeavesItsOwnersRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := seededRemoteURL(t, ctx)
	st, heads := cloneHeldRemote(t, ctx, "a/b", url)
	if err := st.SyncAddRemote(ctx, remoteName(t, "a"), url); err != nil {
		t.Fatalf("SyncAddRemote(a): %v", err)
	}
	held := map[string]string{"b/master": heads["a/b/master"]}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/"); !maps.Equal(tracked, held) {
		t.Fatalf("a's tracking refs = %v, want a/b's b/master %v, as Dolt reads it", tracked, held)
	}

	if err := st.SyncRemoveRemote(ctx, "a/b"); err != nil {
		t.Fatalf("SyncRemoveRemote(a/b): %v", err)
	}
	want := []storage.RemoteRemovalRecord{{Remote: "a/b", Owner: "a", Held: true, OwnerHeld: true}}
	if removals := st.RemoteRemovals(); !slices.Equal(removals, want) {
		t.Errorf("RemoteRemovals() = %+v, want %+v", removals, want)
	}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/"); !maps.Equal(tracked, held) {
		t.Errorf("tracking refs under remotes/a/ after removing a/b = %v, want a's %v", tracked, held)
	}
	if forkPoints := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"a/"); !maps.Equal(forkPoints, held) {
		t.Errorf("fork points under lit-fork-point/a/ after removing a/b = %v, want a's %v", forkPoints, held)
	}

	if err := st.SyncFetch(ctx, remoteName(t, "a")); err != nil {
		t.Fatalf("SyncFetch(a): %v", err)
	}
	wantPruned := []storage.PrunedRef{{Remote: "a", Branch: "b/master", Head: held["b/master"]}}
	if pruned := st.PrunedRefs(); !slices.Equal(pruned, wantPruned) {
		t.Errorf("PrunedRefs() = %+v, want %+v", pruned, wantPruned)
	}
	tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/")
	if _, ok := tracked["b/master"]; ok || tracked["master"] == "" || len(tracked) != 1 {
		t.Errorf("tracking refs under remotes/a/ after the fetch = %v, want a's own master alone", tracked)
	}
	if forkPoints := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"a/"); len(forkPoints) != 0 {
		t.Errorf("fork points under lit-fork-point/a/ after the fetch = %v, want b/master's gone and none recorded for master yet", forkPoints)
	}
}

func seededRemoteURL(t *testing.T, ctx context.Context) string {
	t.Helper()
	url := "file://" + filepath.Join(t.TempDir(), "remote")
	seedReconcileRemote(t, ctx, migratedDoltDir(t), url)
	return url
}
