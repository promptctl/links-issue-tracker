package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// [LAW:single-enforcer] All remote-transport failure classification and retry
// live here. This is deliberately NOT part of the online-GC contention retry in
// commit_lock.go: that loop answers "is this GC contention" on a ~25s budget of
// millisecond-scale attempts with a connection rotation between them, while a
// dropped network connection wants a few second-scale attempts and no rotation
// (the connection is not poisoned). Two unrelated recovery policies must not
// share one budget, so they share no code path beyond the retry function types.
//
// Classification matches the transport tool's own stderr (OpenSSH / git),
// never Dolt's rendered English: Dolt's gitauth layer wraps every "fatal:
// could not read from remote repository" — which all SSH transport failures
// print — into an authentication error, and matching that prose would both
// inherit the misclassification and break on the next Dolt rewording. The ssh
// symptom lines survive verbatim through every wrapper, so they are the one
// signal that identifies a network failure at any layer.
// [FRAMING:representation] the wrapper's auth prose is a map that lies about
// this territory; the transport symptom is the territory.

// remoteIORetryMaxAttempts bounds attempts at remote I/O whose failures look
// transient (~7s of waiting: 1s, 2s, 4s between four attempts). Sized for the
// observed failure mode — a dropped SSH connection that succeeds on a plain
// retry seconds later — and kept small deliberately: if the resets are the
// remote throttling this workspace's push cadence, a long retry storm makes
// that worse, not better (links-sync-r779, "Not verified").
//
// A package variable (the delays stay const) so tests whose premise makes
// exhaustion CERTAIN can shrink the budget instead of sleeping through the
// production one — the same convention transientRetryMaxAttempts serves.
var remoteIORetryMaxAttempts = 4

const (
	remoteIORetryBaseDelay = 1 * time.Second
	remoteIORetryMaxDelay  = 4 * time.Second
)

// RemoteUnreachableError is the terminal outcome of remote I/O whose transport
// failed on every attempt in the retry budget: the network path to the remote
// is down, not this workspace's credentials or data. It exists because the
// backend renders every SSH transport failure through its authentication-error
// prose ("git authentication required…"), sending the operator to check keys
// and agents that are fine; this type carries the transport truth instead.
// [LAW:types-are-the-program] the network/auth distinction is carried by the
// type the surface dispatches on, not re-derived from message text at the CLI.
type RemoteUnreachableError struct {
	// Attempts is how many times the operation was tried before giving up.
	Attempts int
	// Symptom is the transport tool's own failure line, verbatim (e.g.
	// "ssh: connect to host github.com port 22: Connection refused").
	Symptom string
	// Cause is the final attempt's full error, preserved for diagnosis.
	Cause error
}

func (e RemoteUnreachableError) Error() string {
	// The symptom is the headline and the auth misdiagnosis is disclaimed in the
	// same breath: the backend's rendering of this failure claims an
	// authentication problem, and an operator who has seen that prose needs the
	// correction where they are looking. The misleading backend text itself is
	// deliberately not echoed here; it stays reachable through Unwrap.
	return fmt.Sprintf(
		"remote unreachable: %s (the network transport failed on all %d attempts; this is not an authentication problem)",
		e.Symptom, e.Attempts)
}

// Unwrap preserves the full backend cause chain for diagnosis.
func (e RemoteUnreachableError) Unwrap() error { return e.Cause }

// remoteTransportSymptomPatterns enumerates the transport-tool stderr shapes
// that mean "the network path failed" — conditions a retry can outwait. The
// accept set is deliberately tight: auth failures ("Permission denied
// (publickey)", "Authentication failed", "Host key verification failed") and
// content failures (branch not found, non-fast-forward) match nothing here and
// must never be retried — retrying a deterministic refusal only delays the
// truthful report. [LAW:parse-dont-validate] matched once, stamped as
// RemoteUnreachableError; nothing downstream re-reads the text.
var remoteTransportSymptomPatterns = []string{
	"connection refused",
	"connection reset",
	"connection timed out",
	"operation timed out",
	"kex_exchange_identification",
	"network is unreachable",
	"no route to host",
	"temporary failure in name resolution",
}

// remoteTransportSymptom reports whether err carries a transient remote
// transport failure, and when it does, returns the transport tool's own
// failure line — the first line of the error text containing a known symptom —
// so the eventual report names the concrete symptom rather than a category.
func remoteTransportSymptom(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		lowered := strings.ToLower(line)
		for _, pattern := range remoteTransportSymptomPatterns {
			if strings.Contains(lowered, pattern) {
				return strings.TrimSpace(line), true
			}
		}
	}
	return "", false
}

// retryTransientRemoteIO runs operation, and on a transient remote-transport
// failure backs off and retries within the remote-I/O budget. Any other error
// returns unchanged on the attempt that produced it — including a genuine auth
// failure arriving mid-budget, which must surface as itself, not as
// exhaustion. A transport failure that survives the whole budget is promoted
// to RemoteUnreachableError, so only a failure the retry could not absorb ever
// reaches the user. [LAW:no-silent-failure] the retries hide nothing terminal:
// exhaustion reports the symptom, the attempt count, and the full cause chain.
func retryTransientRemoteIO(ctx context.Context, operation retryOperation, delayForAttempt retryDelayFunc, sleep retrySleepFunc) error {
	var lastErr error
	var lastSymptom string
	for attempt := 1; attempt <= remoteIORetryMaxAttempts; attempt++ {
		err := operation(ctx)
		if err == nil {
			return nil
		}
		symptom, transient := remoteTransportSymptom(err)
		if !transient {
			return err
		}
		lastErr, lastSymptom = err, symptom
		if attempt == remoteIORetryMaxAttempts {
			break
		}
		if waitErr := sleep(ctx, delayForAttempt(attempt)); waitErr != nil {
			return waitErr
		}
	}
	return RemoteUnreachableError{Attempts: remoteIORetryMaxAttempts, Symptom: lastSymptom, Cause: lastErr}
}

func remoteIORetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := remoteIORetryBaseDelay << (attempt - 1)
	if delay > remoteIORetryMaxDelay {
		delay = remoteIORetryMaxDelay
	}
	return delay
}

// runRemoteIO is the production spelling of retryTransientRemoteIO — one place
// binds the real delay schedule and sleeper, so the remote-touching call sites
// (push, fetch) cannot drift on retry policy. [LAW:one-source-of-truth]
func runRemoteIO(ctx context.Context, operation retryOperation) error {
	return retryTransientRemoteIO(ctx, operation, remoteIORetryDelay, waitWithContext)
}
