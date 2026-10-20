package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Only a push the remote refused on its history can be a re-root, so only a
// refusal is fetched, followed and retried. A refusal is recognized by the
// texts DOLT_PUSH's two refusals carry; every other failure — the transport
// giving out, a remote that never opened, a write the remote's storage
// failed, the caller's context ending, a lock or compaction that failed
// before any push — passes through untouched, with nothing fetched: the zero
// Store here has no engine, so a fetch would panic. The failures' texts are
// the ones DOLT_PUSH returned for each, measured.
func TestOnlyARefusedPushIsFollowedAndRetried(t *testing.T) {
	live := context.Background()
	noAncestor := errors.New("Error 1105: unknown push error; no common ancestor")
	for _, tc := range []struct {
		name        string
		pushErr     error
		wantRefused bool
	}{
		{"refused, no common ancestor", noAncestor, true},
		{"refused, non-fast-forward", errors.New("Error 1105: To file:///r\n ! [rejected]            HEAD -> master (non-fast-forward)\nerror: failed to push some refs to 'file:///r'\nhint: Updates were rejected because the tip of your current branch is behind\n"), true},
		{"transport gave out", RemoteUnreachableError{Attempts: 3, Symptom: "connection refused", Cause: noAncestor}, false},
		{"remote never opened", errors.New("Error 1105: failed to get remote db; the remote: gone 'file:///r' could not be accessed; operation not valid on a file"), false},
		{"remote not configured", errors.New("Error 1105: fatal: remote 'no-such-remote' not found.\nPlease make sure the remote exists."), false},
		{"remote's storage failed the write", errors.New("Error 1105: unknown push error; open /r/.git permission denied"), false},
		{"git remote declined the write", errors.New("Error 1105: unknown push error; git command failed (exit 1)\ncommand: git push --porcelain --force-with-lease=refs/dolt/data:abc origin refs/dolt/blobstore:refs/dolt/data\noutput:\nTo /r\n!\trefs/dolt/blobstore:refs/dolt/data\t[remote rejected] (pre-receive hook declined)\nDone\nerror: failed to push some refs to '/r'"), false},
		{"context ended", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifyPushFailure("origin", tc.pushErr)
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
		classifyPushFailure("origin", RemoteUnreachableError{Attempts: 3, Symptom: "connection refused", Cause: noAncestor}),
		classifyPushFailure("origin", errors.New("Error 1105: failed to get remote db; the remote: gone 'file:///r' could not be accessed; operation not valid on a file")),
		classifyPushFailure("origin", errors.New("Error 1105: unknown push error; open /r/.git permission denied")),
	} {
		pushed := storage.SyncPushResult{Head: "h"}
		result, gotErr, _, followErr := (&Store{}).retryPushAfterFollow(live, "origin", "master", false, pushed, pushErr)
		if result != pushed || gotErr != pushErr || followErr != nil {
			t.Fatalf("retryPushAfterFollow(%v) = %+v, %v, %v; want the push's own outcome untouched", pushErr, result, gotErr, followErr)
		}
	}
}

// The rejection hint is Dolt's: a reworded ErrFailedToPush fails here rather
// than silently turning refusals into plain failures. (The other refusal text
// is read from doltdb.ErrNoCommonAncestor itself.)
func TestPushRefusalTextsAreDoltsOwn(t *testing.T) {
	if rejected := doltenv.ErrFailedToPush.New("file:///r").Error(); !strings.Contains(rejected, pushRejectedHint) {
		t.Fatalf("Dolt's ErrFailedToPush is %q; it no longer carries the refusal text %q", rejected, pushRejectedHint)
	}
}
