package store

import (
	"context"
	"maps"
	"path/filepath"
	"strings"
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

// TestParseRemoteNameAdmitsWhatDoltHolds: Dolt adds every name the parse
// admits and refuses every name it refuses, so the parse is Dolt's own rule,
// said where lit names the remote.
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

// TestTheStoreRefusesARemoteDoltCannotHold: every verb that names a remote
// refuses a/b with the parse's reason, never Dolt's bare "remote name invalid".
func TestTheStoreRefusesARemoteDoltCannotHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	url := "file://" + filepath.Join(t.TempDir(), "remote")
	for verb, err := range map[string]error{
		"SyncAddRemote":    st.SyncAddRemote(ctx, "a/b", url),
		"SyncFetch":        st.SyncFetch(ctx, "a/b", false),
		"SyncSetRemoteURL": st.SyncSetRemoteURL(ctx, "a/b", url),
		"SyncRemoveRemote": st.SyncRemoveRemote(ctx, "a/b"),
	} {
		if err == nil || !strings.Contains(err.Error(), `remote "a/b" has "/" in its name`) {
			t.Errorf("%s(a/b) error = %v, want the refusal naming a/b", verb, err)
		}
	}
}

// fetchedRemote adds name through the store and fetches it twice, so the
// second fetch records the fork point of every branch the first one brought.
// It returns the tracking refs' heads, keyed by branch.
func fetchedRemote(t *testing.T, ctx context.Context, st *Store, name, url string) map[string]string {
	t.Helper()
	if err := st.SyncAddRemote(ctx, name, url); err != nil {
		t.Fatalf("SyncAddRemote(%s): %v", name, err)
	}
	if err := st.SyncFetch(ctx, name, false); err != nil {
		t.Fatalf("SyncFetch(%s): %v", name, err)
	}
	if err := st.SyncResetToRemoteHead(ctx, name, "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead(%s): %v", name, err)
	}
	if err := st.SyncFetch(ctx, name, false); err != nil {
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
