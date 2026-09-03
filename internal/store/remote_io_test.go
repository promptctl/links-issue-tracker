package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The two accept fixtures are verbatim from the sync traces that motivated
// links-sync-r779: a real dropped connection and a real mid-handshake reset,
// both wrapped the way the backend's auth-normalizing layer renders them —
// auth prose first, transport symptom buried in the git output.
const traceConnectionRefused = `git authentication required but interactive prompting is disabled

Hints:
- HTTPS: configure git credentials (credential helper, token) ahead of time
- SSH: use ssh-agent / keychain and verify ` + "`ssh -o BatchMode=yes <host>`" + ` works
- GCM: ensure non-interactive auth is configured

Git output:
ssh: connect to host github.com port 22: Connection refused
fatal: Could not read from remote repository.
Original error: git command failed (exit 128)`

const traceConnectionReset = `kex_exchange_identification: read: Connection reset by peer
Connection reset by 140.82.113.4 port 22
fatal: Could not read from remote repository.
Original error: git ls-remote --heads -- origin failed: exit status 128`

func TestRemoteTransportSymptomClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantSymptom string
		wantOK      bool
	}{
		{
			// The observed defect: the transport symptom must win over the auth
			// prose it is wrapped in, and the returned line names the concrete
			// symptom, host and port included.
			"dropped connection wrapped as auth failure",
			errors.New("Error 1105: failed to get remote db: " + traceConnectionRefused),
			"ssh: connect to host github.com port 22: Connection refused",
			true,
		},
		{
			"mid-handshake reset",
			errors.New(traceConnectionReset),
			"kex_exchange_identification: read: Connection reset by peer",
			true,
		},
		{"timeout", errors.New("ssh: connect to host github.com port 22: Operation timed out"), "ssh: connect to host github.com port 22: Operation timed out", true},
		{"unreachable network", errors.New("ssh: connect to host github.com port 22: Network is unreachable"), "ssh: connect to host github.com port 22: Network is unreachable", true},
		{"transient dns failure", errors.New("ssh: Could not resolve hostname github.com: Temporary failure in name resolution"), "ssh: Could not resolve hostname github.com: Temporary failure in name resolution", true},
		// The reject set: deterministic refusals a retry can never outwait.
		{"nil", nil, "", false},
		{"genuine auth failure", errors.New("git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository."), "", false},
		{"host key verification", errors.New("Host key verification failed.\nfatal: Could not read from remote repository."), "", false},
		{"https auth failure", errors.New("fatal: Authentication failed for 'https://github.com/x/y.git/'"), "", false},
		{"content failure", errors.New("Error 1105: branch not found: nosuch"), "", false},
		{"gc contention is not remote transport", errors.New("cannot update manifest: database is read only"), "", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			symptom, ok := remoteTransportSymptom(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("remoteTransportSymptom ok = %v, want %v", ok, tc.wantOK)
			}
			if symptom != tc.wantSymptom {
				t.Fatalf("symptom = %q, want %q", symptom, tc.wantSymptom)
			}
		})
	}
}

func TestRetryTransientRemoteIORetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	op := &fakeRetryOperation{
		results: []error{
			errors.New(traceConnectionRefused),
			nil,
		},
	}
	err := retryTransientRemoteIO(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil {
		t.Fatalf("retryTransientRemoteIO() error = %v", err)
	}
	if op.calls != 2 {
		t.Fatalf("op.calls = %d, want 2", op.calls)
	}
}

// A non-transport failure — a genuine auth refusal included — returns
// unchanged on the attempt that produced it: retrying a deterministic refusal
// only delays the truthful report, and exhaustion must not relabel it.
func TestRetryTransientRemoteIOPassesNonTransportErrorThrough(t *testing.T) {
	t.Parallel()
	authErr := errors.New("git@github.com: Permission denied (publickey).")
	op := &fakeRetryOperation{results: []error{authErr}}
	err := retryTransientRemoteIO(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if !errors.Is(err, authErr) {
		t.Fatalf("error = %v, want the auth error unchanged", err)
	}
	var unreachable RemoteUnreachableError
	if errors.As(err, &unreachable) {
		t.Fatalf("non-transport error must not be relabeled RemoteUnreachableError: %v", err)
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1 (no retry of a deterministic refusal)", op.calls)
	}
}

func TestRetryTransientRemoteIOExhaustionReportsTransportTruth(t *testing.T) {
	t.Parallel()
	results := make([]error, 0, remoteIORetryMaxAttempts)
	for attempt := 0; attempt < remoteIORetryMaxAttempts; attempt++ {
		results = append(results, errors.New(traceConnectionRefused))
	}
	op := &fakeRetryOperation{results: results}
	err := retryTransientRemoteIO(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return nil },
	)
	if op.calls != remoteIORetryMaxAttempts {
		t.Fatalf("op.calls = %d, want %d", op.calls, remoteIORetryMaxAttempts)
	}
	var unreachable RemoteUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("error = %v, want RemoteUnreachableError", err)
	}
	if unreachable.Attempts != remoteIORetryMaxAttempts {
		t.Fatalf("Attempts = %d, want %d", unreachable.Attempts, remoteIORetryMaxAttempts)
	}
	if unreachable.Symptom != "ssh: connect to host github.com port 22: Connection refused" {
		t.Fatalf("Symptom = %q", unreachable.Symptom)
	}
	// The rendered message leads with the transport symptom and corrects the
	// auth misdiagnosis; the misleading backend prose is not echoed but stays
	// reachable through the cause chain for diagnosis. [LAW:no-silent-failure]
	msg := err.Error()
	if !strings.HasPrefix(msg, "remote unreachable: ssh: connect to host github.com port 22: Connection refused") {
		t.Fatalf("message does not lead with the transport symptom: %q", msg)
	}
	if unwrapped := errors.Unwrap(err); unwrapped == nil || unwrapped.Error() != traceConnectionRefused {
		t.Fatalf("cause chain lost: %v", unwrapped)
	}
}

func TestRetryTransientRemoteIOAbortsOnCanceledSleep(t *testing.T) {
	t.Parallel()
	op := &fakeRetryOperation{results: []error{errors.New(traceConnectionRefused)}}
	err := retryTransientRemoteIO(
		context.Background(),
		op.run,
		func(int) time.Duration { return 0 },
		func(context.Context, time.Duration) error { return context.Canceled },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if op.calls != 1 {
		t.Fatalf("op.calls = %d, want 1", op.calls)
	}
}
