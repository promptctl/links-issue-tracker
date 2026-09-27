package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The received-refs record is what the last settled automatic receive learned
// about the sync remote — the advertisement it observed before it fetched —
// kept at <StorageDir>/received-refs.last so the receive can answer "has the
// remote moved" with the store closed (internal/cli/sync_receive_ask.go owns
// the question and the record's bytes). It is knowledge ABOUT the Dolt
// directory that sits beside it, so it must live and die with that directory:
// a record that outlived a `lit snapshots restore` to an older snapshot would
// say the store holds a remote head it no longer holds, and every later
// receive would find the remote "unmoved" and never fetch it back. Every
// directory rotator takes the exclusive workspace lock, so the lock forgets
// the record as the hold is taken (LockWorkspaceExclusive); the record can
// then lag the directory beside it but never lead it. [LAW:single-enforcer]
// one boundary every rotation crosses, so no rotator can forget to forget.
const receivedRefsMarkerName = "received-refs.last"

// ReceivedRefsPath is the record's path for a Dolt root directory: a sibling of
// the directory, in the same position as the locks, so a rotation of the
// directory neither moves nor copies it. [LAW:one-source-of-truth]
func ReceivedRefsPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), receivedRefsMarkerName)
}

// ReadReceivedRefs returns the record, nil when no receive has recorded one.
// Any other read failure is the caller's to weigh. [LAW:parse-dont-validate]
// "absent" is a value here, not an error: the receive treats it as "fetch".
func ReadReceivedRefs(databasePath string) ([]byte, error) {
	payload, err := os.ReadFile(ReceivedRefsPath(databasePath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return payload, err
}

// WriteReceivedRefs replaces the record atomically (write beside it, rename
// over it), so a reader never sees a torn record and a crash leaves either the
// old record or the new one.
func WriteReceivedRefs(databasePath string, payload []byte) error {
	target := ReceivedRefsPath(databasePath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for received-refs record: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(target), receivedRefsMarkerName+".tmp-*")
	if err != nil {
		return fmt.Errorf("write received-refs record: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		return errors.Join(fmt.Errorf("write received-refs record: %w", err), f.Close(), os.Remove(f.Name()))
	}
	if err := f.Close(); err != nil {
		return errors.Join(fmt.Errorf("close received-refs record: %w", err), os.Remove(f.Name()))
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return errors.Join(fmt.Errorf("install received-refs record: %w", err), os.Remove(f.Name()))
	}
	return nil
}

// forgetReceivedRefs removes the record; an already-absent record is the
// desired end state, not a failure. Any other failure is returned: a rotation
// that cannot forget what the old directory knew must not start, or the bug
// this record's lifecycle exists to prevent comes back silently.
// [LAW:no-silent-failure]
func forgetReceivedRefs(databasePath string) error {
	if err := os.Remove(ReceivedRefsPath(databasePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("forget received-refs record before rotating the Dolt directory: %w", err)
	}
	return nil
}
