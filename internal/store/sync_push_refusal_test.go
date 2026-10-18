package store

import (
	"context"
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Only a push the remote refused can be a re-root, so only a refusal is
// fetched, followed and retried. A failure that never reached a verdict from
// the remote — the transport giving out, the caller's context ending, a lock
// or compaction that failed before any push — passes through untouched, with
// nothing fetched: the zero Store here has no engine, so a fetch would panic.
func TestOnlyARefusedPushIsFollowedAndRetried(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	cause := errors.New("unknown push error; no common ancestor")
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		pushErr     error
		wantRefused bool
	}{
		{"remote refused", live, cause, true},
		{"transport gave out", live, RemoteUnreachableError{Attempts: 3, Symptom: "connection refused", Cause: cause}, false},
		{"context ended", ended, context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifyPushFailure(tc.ctx, "origin", tc.pushErr)
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
	for _, pushErr := range []error{nil, lockTimeout, classifyPushFailure(live, "origin", RemoteUnreachableError{Attempts: 3, Symptom: "connection refused", Cause: cause})} {
		pushed := storage.SyncPushResult{Head: "h"}
		result, gotErr, _, followErr := (&Store{}).retryPushAfterFollow(live, "origin", "master", false, false, pushed, pushErr)
		if result != pushed || gotErr != pushErr || followErr != nil {
			t.Fatalf("retryPushAfterFollow(%v) = %+v, %v, %v; want the push's own outcome untouched", pushErr, result, gotErr, followErr)
		}
	}
}
