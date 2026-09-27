package store

import (
	"context"
	"os"
	"testing"
)

// TestExclusiveWorkspaceLockForgetsReceivedRefs pins the record's lifecycle:
// taking the exclusive hold — the one act every Dolt-directory rotator
// performs — removes the received-refs record, while the shared hold every
// reader takes leaves it alone. Without the first half, a `lit snapshots
// restore` to an older snapshot would sit beside a record claiming the store
// holds a remote head it no longer holds, and the automatic receive would
// report the remote unmoved instead of fetching it back.
// [LAW:behavior-not-structure] asserted on the file, not on who removed it.
func TestExclusiveWorkspaceLockForgetsReceivedRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := t.TempDir() + "/dolt"
	if err := os.MkdirAll(doltRoot, 0o755); err != nil {
		t.Fatalf("mkdir dolt root error = %v", err)
	}
	want := []byte("origin git@example.invalid:o/r.git\nabc\trefs/dolt/data\n")
	if err := WriteReceivedRefs(doltRoot, want); err != nil {
		t.Fatalf("WriteReceivedRefs() error = %v", err)
	}

	releaseShared, err := LockWorkspaceShared(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceShared() error = %v", err)
	}
	got, err := ReadReceivedRefs(doltRoot)
	if err != nil || string(got) != string(want) {
		t.Fatalf("record after a shared hold = %q, %v; want %q, nil", got, err, want)
	}
	if err := releaseShared(); err != nil {
		t.Fatalf("release shared error = %v", err)
	}

	releaseExclusive, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive() error = %v", err)
	}
	got, err = ReadReceivedRefs(doltRoot)
	if err != nil || got != nil {
		t.Fatalf("record while the exclusive hold is taken = %q, %v; want nil, nil", got, err)
	}
	if err := releaseExclusive(); err != nil {
		t.Fatalf("release exclusive error = %v", err)
	}

	// Absent is the end state, not a failure: a second rotation with nothing to
	// forget must still take its hold.
	releaseExclusive, err = LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive() with no record error = %v", err)
	}
	if err := releaseExclusive(); err != nil {
		t.Fatalf("release exclusive error = %v", err)
	}
}
