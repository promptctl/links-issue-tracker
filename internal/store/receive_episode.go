package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
)

// The receive episode is told by two timestamp records beside the Dolt
// directory, each with exactly one writer, and nothing reads either one to
// write it back:
//
//   - settled.last: when a settle last succeeded. Written only by
//     settleReceived, the one function every settle runs through (the
//     automatic receive, the mirror's receive of its re-rooted push, and
//     `lit sync pull`).
//   - unsettled.first: when the current run of post-fetch stops began.
//     Written only by the automatic receive, under the receive lock.
//
// Whether the receive has been failing for a while is derived from the two,
// never stored, so a settle that lands at any moment beside a stopping
// receive is never lost. [LAW:one-source-of-truth] [LAW:single-enforcer]
//
// Both describe this checkout's receive history ("a settle succeeded at T",
// "receives have stopped since T"), not the contents of the Dolt directory,
// so unlike received-refs.last they are true across a rotation and nothing
// forgets them.
const (
	settledLastName    = "settled.last"
	unsettledFirstName = "unsettled.first"
)

// SettledLastPath is the settled.last record's path for a Dolt root directory,
// a sibling of the directory in the received-refs record's position.
func SettledLastPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), settledLastName)
}

// UnsettledFirstPath is the unsettled.first record's path for a Dolt root
// directory, a sibling of the directory in the received-refs record's position.
func UnsettledFirstPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), unsettledFirstName)
}

// ReadSettledLast is when a settle last succeeded on this checkout; ok=false
// when none ever has. [LAW:parse-dont-validate] absence is a value, not an
// error, and a record that does not parse is an error, never a zero time.
func ReadSettledLast(databasePath string) (at time.Time, ok bool, err error) {
	return readTimestampRecord(SettledLastPath(databasePath))
}

// ReadUnsettledFirst is when the automatic receive's current run of post-fetch
// stops began; ok=false when no stop was ever recorded.
func ReadUnsettledFirst(databasePath string) (at time.Time, ok bool, err error) {
	return readTimestampRecord(UnsettledFirstPath(databasePath))
}

// WriteUnsettledFirst records that a run of post-fetch stops began at at. Its
// one caller is the automatic receive, which holds the receive lock.
func WriteUnsettledFirst(databasePath string, at time.Time) error {
	return writeTimestampRecord(UnsettledFirstPath(databasePath), at)
}

// writeSettledLast records a settle that succeeded at at. It is unexported so
// that settleReceived stays its only writer. [LAW:single-enforcer]
func writeSettledLast(databasePath string, at time.Time) error {
	return writeTimestampRecord(SettledLastPath(databasePath), at)
}

func readTimestampRecord(path string) (time.Time, bool, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(payload)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return at, true, nil
}

// writeTimestampRecord replaces the record atomically, so a reader never sees
// a torn timestamp.
func writeTimestampRecord(path string, at time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure storage dir for %s: %w", filepath.Base(path), err)
	}
	if err := atomicfile.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o600, atomicfile.Replaceable); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
