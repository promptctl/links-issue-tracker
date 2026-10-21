package store

import (
	"context"
	"testing"
)

// observeDolt opens the database at doltRoot read-only and below the migration
// gate — the connection DumpRaw reads through — and hands that store to fn. It is how a
// test reads a store's version-control state (HEAD, commit log, branches) while
// the store is closed or while Open refuses it, in process rather than through
// an external dolt binary.
func observeDolt(t *testing.T, ctx context.Context, doltRoot, workspaceID string, fn func(s *Store)) {
	t.Helper()
	s, err := openStoreConnection(ctx, testEnv(), doltRoot, workspaceID, engineRead)
	if err != nil {
		t.Fatalf("open %s read-only to observe it: %v", doltRoot, err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("close observer of %s: %v", doltRoot, err)
		}
	}()
	fn(s)
}

// observedHead is the HEAD commit of the closed store at doltRoot.
func observedHead(t *testing.T, ctx context.Context, doltRoot, workspaceID string) string {
	t.Helper()
	var head string
	observeDolt(t, ctx, doltRoot, workspaceID, func(s *Store) {
		var err error
		if head, err = readDoltHead(ctx, s.db); err != nil {
			t.Fatal(err)
		}
	})
	return head
}

// observedBranch is the branch the closed store at doltRoot has checked out.
func observedBranch(t *testing.T, ctx context.Context, doltRoot, workspaceID string) string {
	t.Helper()
	var branch string
	observeDolt(t, ctx, doltRoot, workspaceID, func(s *Store) {
		var err error
		if branch, err = activeBranch(ctx, s.db); err != nil {
			t.Fatal(err)
		}
	})
	return branch
}

// observedCommitMessages is every commit message on the branch the closed store
// at doltRoot has checked out, newest first.
func observedCommitMessages(t *testing.T, ctx context.Context, doltRoot, workspaceID string) []string {
	t.Helper()
	var messages []string
	observeDolt(t, ctx, doltRoot, workspaceID, func(s *Store) {
		rows, err := s.db.QueryContext(ctx, `SELECT message FROM dolt_log()`)
		if err != nil {
			t.Fatalf("read commit log: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				t.Fatalf("read commit log: %v", err)
			}
			messages = append(messages, m)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read commit log: %v", err)
		}
	})
	return messages
}
