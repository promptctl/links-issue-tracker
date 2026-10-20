package store

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"

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
	if err := st.SyncFetch(ctx, remoteName(t, name), false); err != nil {
		t.Fatalf("SyncFetch(%s): %v", name, err)
	}
	if err := st.SyncResetToRemoteHead(ctx, remoteName(t, name), "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead(%s): %v", name, err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, name), false); err != nil {
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

	if err := st.SyncRemoveRemote(ctx, "a"); err != nil {
		t.Fatalf("SyncRemoveRemote(a): %v", err)
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
	db, err := openDoltPool(root, "ws", "", engineWrite)
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

	if err := st.SyncAddRemote(ctx, remoteName(t, "team"), url); err != nil {
		t.Fatalf("SyncAddRemote(team): %v", err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, "team"), false); err != nil {
		t.Fatalf("SyncFetch(team): %v", err)
	}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/team/"); len(tracked) != 1 || tracked["master"] != heads["team/origin/master"] {
		t.Errorf("tracking refs under remotes/team/ after team's fetch = %v, want only master at %s", tracked, heads["team/origin/master"])
	}
}

// TestRemovingACloneHeldSlashNamedRemoteLeavesItsOwnersRefs: a/b's refs are
// a's refs for branches b/*, so removing a clone-held a/b beside a drops none
// of them, and a's pruning fetch then leaves exactly a's own branches and
// their fork points.
func TestRemovingACloneHeldSlashNamedRemoteLeavesItsOwnersRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := seededRemoteURL(t, ctx)
	st, _ := cloneHeldRemote(t, ctx, "a/b", url)
	aHeads := fetchedRemote(t, ctx, st, "a", url)
	if _, ok := aHeads["b/master"]; !ok {
		t.Fatalf("a's tracking refs = %v, want a/b's b/master among them, as Dolt reads it", aHeads)
	}

	if err := st.SyncRemoveRemote(ctx, "a/b"); err != nil {
		t.Fatalf("SyncRemoveRemote(a/b): %v", err)
	}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/"); !maps.Equal(tracked, aHeads) {
		t.Errorf("tracking refs under remotes/a/ after removing a/b = %v, want a's %v", tracked, aHeads)
	}
	if forkPoints := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"a/"); !maps.Equal(forkPoints, aHeads) {
		t.Errorf("fork points under lit-fork-point/a/ after removing a/b = %v, want a's %v", forkPoints, aHeads)
	}

	if err := st.SyncFetch(ctx, remoteName(t, "a"), true); err != nil {
		t.Fatalf("SyncFetch(a, prune): %v", err)
	}
	own := map[string]string{"master": aHeads["master"]}
	if tracked := headsUnder(t, ctx, st, `dolt_remote_branches`, "remotes/a/"); !maps.Equal(tracked, own) {
		t.Errorf("tracking refs under remotes/a/ after the prune = %v, want a's own %v", tracked, own)
	}
	if forkPoints := headsUnder(t, ctx, st, `dolt_branches`, forkPointPrefix+"a/"); !maps.Equal(forkPoints, own) {
		t.Errorf("fork points under lit-fork-point/a/ after the prune = %v, want a's own %v", forkPoints, own)
	}
}

func seededRemoteURL(t *testing.T, ctx context.Context) string {
	t.Helper()
	url := "file://" + filepath.Join(t.TempDir(), "remote")
	seedReconcileRemote(t, ctx, migratedDoltDir(t), url)
	return url
}
