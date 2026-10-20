package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/store/datas"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Only a push the remote refused on its history can be a re-root, so only a
// refusal is fetched, followed and retried. A refusal is recognized by the
// Dolt error DOLT_PUSH wraps for it; every other failure — a stale lease, the
// transport giving out, a remote that never opened, a write the remote's
// storage failed, the caller's context ending, a lock or compaction that
// failed before any push — passes through untouched, with nothing fetched:
// the zero Store here has no engine, so a fetch would panic. The refusals are
// built as DOLT_PUSH wraps them; the other failures' texts are the ones
// DOLT_PUSH returned for each, measured.
func TestOnlyARefusedPushIsFollowedAndRetried(t *testing.T) {
	live := context.Background()
	noAncestor := fmt.Errorf("%w; %w", actions.ErrUnknownPushErr, doltdb.ErrNoCommonAncestor)
	rejected := func(cause error) error {
		return fmt.Errorf("To file:///r\n ! [rejected]            HEAD -> master\n%w", fmt.Errorf("%w: %w", doltenv.ErrFailedToPush.New("file:///r"), cause))
	}
	for _, tc := range []struct {
		name        string
		pushErr     error
		wantRefused bool
	}{
		{"refused, no common ancestor", noAncestor, true},
		{"refused, can't fast-forward", rejected(actions.ErrCantFF), true},
		{"refused, remote is ahead", rejected(doltdb.ErrIsAhead), true},
		{"refused, merge needed", rejected(datas.ErrMergeNeeded), true},
		{"stale lease", rejected(datas.ErrStaleInfo), false},
		{"transport gave out", RemoteUnreachableError{Attempts: 3, Symptom: "connection refused", Cause: noAncestor}, false},
		{"remote never opened", errors.New("Error 1105: failed to get remote db; the remote: gone 'file:///r' could not be accessed; operation not valid on a file"), false},
		{"remote not configured", errors.New("Error 1105: fatal: remote 'no-such-remote' not found.\nPlease make sure the remote exists."), false},
		{"remote's storage failed the write", errors.New("Error 1105: unknown push error; open /r/.git permission denied"), false},
		{"git remote declined the write", errors.New("Error 1105: unknown push error; git command failed (exit 1)\ncommand: git push --porcelain --force-with-lease=refs/dolt/data:abc origin refs/dolt/blobstore:refs/dolt/data\noutput:\nTo /r\n!\trefs/dolt/blobstore:refs/dolt/data\t[remote rejected] (pre-receive hook declined)\nDone\nerror: failed to push some refs to '/r'"), false},
		{"context ended", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifyPushFailure(remoteName(t, "origin"), tc.pushErr)
			var refusal pushRefusedError
			if errors.As(classified, &refusal) != tc.wantRefused {
				t.Fatalf("classifyPushFailure(%v) = %#v; refusal %v, want %v", tc.pushErr, classified, !tc.wantRefused, tc.wantRefused)
			}
			if !errors.Is(classified, tc.pushErr) && !errors.As(classified, new(RemoteUnreachableError)) {
				t.Fatalf("classifyPushFailure(%v) = %v; the cause is not kept", tc.pushErr, classified)
			}
			if classified.Error() != `push remote "origin": `+tc.pushErr.Error() {
				t.Fatalf("classifyPushFailure(%v).Error() = %q; want the push's own wording", tc.pushErr, classified.Error())
			}
		})
	}
	lockTimeout := errors.New("acquire commit lock: timed out")
	for _, pushErr := range []error{
		nil,
		lockTimeout,
		classifyPushFailure(remoteName(t, "origin"), RemoteUnreachableError{Attempts: 3, Symptom: "connection refused", Cause: noAncestor}),
		classifyPushFailure(remoteName(t, "origin"), errors.New("Error 1105: failed to get remote db; the remote: gone 'file:///r' could not be accessed; operation not valid on a file")),
		classifyPushFailure(remoteName(t, "origin"), errors.New("Error 1105: unknown push error; open /r/.git permission denied")),
	} {
		pushed := storage.SyncPushResult{Head: "h"}
		result, gotErr, _, followErr := (&Store{}).retryPushAfterFollow(live, remoteName(t, "origin"), "master", false, pushed, pushErr)
		if !reflect.DeepEqual(result, pushed) || gotErr != pushErr || followErr != nil {
			t.Fatalf("retryPushAfterFollow(%v) = %+v, %v, %v; want the push's own outcome untouched", pushErr, result, gotErr, followErr)
		}
	}
}

// A caller's context that ends while Dolt is running the query reaches lit as
// context.Canceled through the driver's error, not as a bare Error 1105, so
// classifyPushOutcome and countsTowardUnsettled read a push or receive torn
// down mid-call as cancelled, the same as one cancelled before it started.
// SLEEP holds the engine inside the call when the cancel lands.
func TestACancelInsideDoltReachesLitAsContextCanceled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	time.AfterFunc(200*time.Millisecond, cancel)
	var slept any
	err := st.db.QueryRowContext(queryCtx, `SELECT SLEEP(30)`).Scan(&slept)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("query cancelled inside Dolt: error = %v (%T), want context.Canceled in the chain", err, err)
	}
}
