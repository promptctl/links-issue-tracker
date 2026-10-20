package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
)

// The received-refs record is the last advertisement of the sync remote this
// store is known to hold: what a settled automatic receive observed before it
// fetched, or what a landed push proved it left the remote showing. It is
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
// A database that appears where there was none is not a rotation: the
// bootstrap's install forgets the record as it puts the new one in place
// (installDoltRoot).
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

// WriteReceivedRefs replaces the record atomically, so a reader never sees a
// torn record. Replaceable: a record a crash cut short no longer matches any
// advertisement, which only costs the fetch it would have saved.
func WriteReceivedRefs(databasePath string, payload []byte) error {
	return writeStorageRecord(ReceivedRefsPath(databasePath), payload)
}

// writeStorageRecord replaces a record in the storage dir atomically, so a
// reader never sees a torn one. [LAW:one-source-of-truth] the one writer of
// every store record beside the Dolt directory.
func writeStorageRecord(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for %s: %w", filepath.Base(path), err)
	}
	if err := atomicfile.WriteFile(path, payload, 0o600, atomicfile.Replaceable); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
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
