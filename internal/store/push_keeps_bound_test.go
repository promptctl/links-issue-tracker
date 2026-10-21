package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/historywindow"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// pushVerb is one of storage.Syncer's pushes, run against the store at root:
// the live store itself, or a clone of it the way the on-change mirror takes
// one.
type pushVerb struct {
	name string
	push func(t *testing.T, ctx, boundLifetime context.Context, root string, window storage.BoundWindow) (*Store, storage.SyncPushResult, error)
}

var pushVerbs = []pushVerb{
	{"SyncCompactAndPush", func(t *testing.T, ctx, boundLifetime context.Context, root string, window storage.BoundWindow) (*Store, storage.SyncPushResult, error) {
		st := openSyncOrFatal(t, ctx, root)
		result, err := st.SyncCompactAndPush(ctx, boundLifetime, remoteName(t, "origin"), "master", window, false, false)
		return st, result, err
	}},
	{"SyncPushFromClone", func(t *testing.T, ctx, boundLifetime context.Context, root string, window storage.BoundWindow) (*Store, storage.SyncPushResult, error) {
		st := openSyncOrFatal(t, ctx, cloneStoreTree(t, ctx, root, t.TempDir()))
		result, err := st.SyncPushFromClone(ctx, boundLifetime, remoteName(t, "origin"), "master", window, false, false)
		return st, result, err
	}},
}

// atTheBound seeds a store and its remote, and commits once more, so the next
// push leaves the remote head at a height a window re-rooting there fires at.
// It returns the store's root and that window.
func atTheBound(t *testing.T, ctx context.Context) (string, storage.BoundWindow) {
	t.Helper()
	remoteURL, _ := gitRemote(t)
	root := migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, root, remoteURL)
	head := commitTitled(t, ctx, root, epic, "past the bound")
	st := openSyncOrFatal(t, ctx, root)
	height := heightOf(t, ctx, st, head)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return root, storage.BoundWindowRead(windowOf(t, height, 3), nil)
}

// TestEveryPushVerbKeepsTheBound pushes through each of storage.Syncer's
// pushes alone, with no call after it, to a remote head the window fires at.
// The push comes back with the bound kept, the remote re-rooted, and its Head
// still the commit it pushed: no push through the contract lands without its
// bound.
func TestEveryPushVerbKeepsTheBound(t *testing.T) {
	t.Parallel()
	for _, verb := range pushVerbs {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root, window := atTheBound(t, ctx)
			st, result, err := verb.push(t, ctx, ctx, root, window)
			defer func() { _ = st.Close() }()
			if err != nil {
				t.Fatalf("%s() error = %v", verb.name, err)
			}
			b := result.Bound
			if !b.Fired() || b.Err != nil || b.Publication != storage.ReRootPublished {
				t.Fatalf("%s() bound = %+v; want it fired and its re-root published", verb.name, b)
			}
			if bounds := st.HistoryBounds(); len(bounds) != 1 || bounds[0].Head != b.Head {
				t.Fatalf("%s() HistoryBounds() = %+v; want the one bound the push kept", verb.name, bounds)
			}
			if result.Head != b.Head {
				t.Fatalf("%s() Head = %s; want %s, the head the push left and the bound re-rooted", verb.name, result.Head, b.Head)
			}
			if remote, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || remote != b.ReRooted.Head {
				t.Fatalf("%s() left the remote at %s (%v); want the re-root %s", verb.name, remote, err, b.ReRooted.Head)
			}
		})
	}
}

// TestAPushWithItsWindowUnreadLandsAndSaysTheBoundWasNotKept: a config that
// names no window costs the bound, never the push.
func TestAPushWithItsWindowUnreadLandsAndSaysTheBoundWasNotKept(t *testing.T) {
	t.Parallel()
	unread := errors.New("config: sync.history_bound: unreadable")
	for _, verb := range pushVerbs {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root, _ := atTheBound(t, ctx)
			st, result, err := verb.push(t, ctx, ctx, root, storage.BoundWindowRead(historywindow.Window{}, unread))
			defer func() { _ = st.Close() }()
			if err != nil || result.Head == "" {
				t.Fatalf("%s() = %+v, %v; want the push landed", verb.name, result, err)
			}
			if want := "history bound not kept: " + unread.Error(); !strings.Contains(result.Maintenance, want) {
				t.Fatalf("%s() maintenance = %q; want it to say %q", verb.name, result.Maintenance, want)
			}
			if bounds := st.HistoryBounds(); len(bounds) != 0 || result.Bound.Publication != storage.ReRootNotPushed {
				t.Fatalf("%s() kept a bound with no window: %+v, %+v", verb.name, bounds, result.Bound)
			}
		})
	}
}

// TestAPushKeepsItsBoundOnTheBoundLifetime: the push runs on its own
// lifetime and the bound on the one it is handed, so a bound lifetime that
// has already ended stops the bound and not the push.
func TestAPushKeepsItsBoundOnTheBoundLifetime(t *testing.T) {
	t.Parallel()
	for _, verb := range pushVerbs {
		t.Run(verb.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root, window := atTheBound(t, ctx)
			ended, cancel := context.WithCancel(ctx)
			cancel()
			st, result, err := verb.push(t, ctx, ended, root, window)
			defer func() { _ = st.Close() }()
			if err != nil || result.Head == "" {
				t.Fatalf("%s() = %+v, %v; want the push landed on its own lifetime", verb.name, result, err)
			}
			if !errors.Is(result.Bound.Err, context.Canceled) || result.Bound.Fired() {
				t.Fatalf("%s() bound = %+v; want it stopped by its ended lifetime before it read the head", verb.name, result.Bound)
			}
		})
	}
}
