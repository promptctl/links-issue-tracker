package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// sourceBuild is the raise policy of a binary built from a working tree: the
// zero policy, which cmd/lit builds for every Origin but release.
var sourceBuild = version.RaisePolicy{}

// releaseBuild is the raise policy of goreleaser's tagged build.
var releaseBuild = version.RaisePolicy{Release: true}

// The raise verdict on the values a push read: a push above the remote's
// schema, or to a remote with no known schema, is a raise and lands only from
// a release; a push at or below it is no raise and lands from any binary.
func TestRefuseSchemaRaise(t *testing.T) {
	t.Parallel()
	origin := remoteName(t, "origin")
	ceiling := ceilingAt(9)
	cases := []struct {
		name    string
		remote  remoteSchema
		commit  int64
		policy  version.RaisePolicy
		refused *SchemaRaiseRequiresReleaseError
	}{
		{"source build at the remote's schema", knownRemoteSchema(9), 9, sourceBuild, nil},
		{"source build below the remote's schema", knownRemoteSchema(9), 8, sourceBuild, nil},
		{"source build above the remote's schema", knownRemoteSchema(8), 9, sourceBuild,
			&SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", RemoteSchema: 8, RemoteKnown: true, CommitSchema: 9, Ceiling: ceiling}},
		{"source build with no remote head", unknownRemoteSchema(), 9, sourceBuild,
			&SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", CommitSchema: 9, Ceiling: ceiling}},
		{"release above the remote's schema", knownRemoteSchema(8), 9, releaseBuild, nil},
		{"release with no remote head", unknownRemoteSchema(), 9, releaseBuild, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := refuseSchemaRaise(origin, "master", tc.remote, tc.commit, tc.policy, ceiling)
			var refused *SchemaRaiseRequiresReleaseError
			if tc.refused == nil {
				if err != nil {
					t.Fatalf("refuseSchemaRaise = %v, want nil", err)
				}
				return
			}
			if !errors.As(err, &refused) {
				t.Fatalf("refuseSchemaRaise = %v, want *SchemaRaiseRequiresReleaseError", err)
			}
			if *refused != *tc.refused {
				t.Fatalf("refusal = %+v, want %+v", *refused, *tc.refused)
			}
		})
	}
}

// The refusal's remedy follows the design: fetch when the remote's schema is
// unknown, a release's push when the schema is released, the downgrade when
// it is not.
func TestSchemaRaiseRefusalNamesTheRemedy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		refused SchemaRaiseRequiresReleaseError
		want    string
	}{
		{"unknown remote", SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", CommitSchema: 9, Ceiling: ceilingAt(9)},
			"fetch first (`lit sync fetch --remote origin`)"},
		{"released schema", SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", RemoteSchema: 8, RemoteKnown: true, CommitSchema: 9, Ceiling: ceilingAt(9)},
			"schema version 9 is released, so a release's `lit sync push` raises the remote"},
		{"unreleased schema", SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", RemoteSchema: 8, RemoteKnown: true, CommitSchema: 9, Ceiling: ceilingAt(8)},
			"`lit downgrade` returns this store to schema version 8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.refused.Error(); !strings.Contains(got, tc.want) {
				t.Fatalf("Error() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// seedOlderSchemaRemote publishes a remote whose head is one schema below
// this tree's registry max, as an older release left it: the store is
// stepped down and pushed by a release. It returns the remote head's hash.
func seedOlderSchemaRemote(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	max := registryMax(t)
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed) = %v", err)
	}
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed) = %v", err)
	}
	head := headCommit(t, ctx, sync)
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(seed) = %v", err)
	}
	return head
}

// adoptAndMigrate adopts the remote's head into root and reopens, which
// migrates the workspace to this tree's registry max: a head one schema
// above the remote's, as a checkout that pulled an older remote holds after
// its next open.
func adoptAndMigrate(t *testing.T, ctx context.Context, root, remoteURL string) {
	t.Helper()
	adoptRemote(t, ctx, root, remoteURL)
	withStore(t, ctx, root, func(*Store) {})
}

// trackingSchema is the schema of origin/master as this store last fetched it.
func trackingSchema(t *testing.T, ctx context.Context, st *Store) remoteSchema {
	t.Helper()
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch = %v", err)
	}
	remoteHead, err := st.trackingHeadSchema(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("trackingHeadSchema = %v", err)
	}
	return remoteHead
}

// A binary built from source never raises a remote: its push over an older
// remote is refused naming both schemas, and the remote keeps its head. The
// same push from a release lands and raises the remote.
func TestASourceBuildNeverRaisesARemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	max := registryMax(t)

	olderHead := seedOlderSchemaRemote(t, ctx, producer, remoteURL)
	adoptAndMigrate(t, ctx, consumer, remoteURL)

	st, err := OpenSync(ctx, testEnv().WithRaisePolicy(sourceBuild), testStderr(), consumer, "ws")
	if err != nil {
		t.Fatalf("OpenSync(consumer) = %v", err)
	}
	defer func() { _ = st.Close() }()
	_, err = st.SyncPush(ctx, remoteName(t, "origin"), "master", false, false)
	var refused *SchemaRaiseRequiresReleaseError
	if !errors.As(err, &refused) {
		t.Fatalf("SyncPush from a source build = %v, want *SchemaRaiseRequiresReleaseError", err)
	}
	want := SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", RemoteSchema: max - 1, RemoteKnown: true, CommitSchema: max, Ceiling: testEnv().ReleasedSchema()}
	if *refused != want {
		t.Fatalf("refusal = %+v, want %+v", *refused, want)
	}
	if head := trackingSchema(t, ctx, st); head != knownRemoteSchema(max-1) {
		t.Fatalf("remote after the refused push = %+v, want schema %d", head, max-1)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", remoteName(t, "origin"), remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused push = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != olderHead {
		t.Fatalf("remote head after the refused push = %s, want %s untouched", got, olderHead)
	}

	st.env = testEnv().WithRaisePolicy(releaseBuild)
	if _, err := st.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush from a release = %v", err)
	}
	if head := trackingSchema(t, ctx, st); head != knownRemoteSchema(max) {
		t.Fatalf("remote after the release's push = %+v, want schema %d", head, max)
	}
}

// A remote this store has never fetched has no schema to judge against, so
// a source build's push to it counts as a raise and is refused, naming the
// fetch; a push that would create the remote is refused the same way, and
// the remote is not created.
func TestASourceBuildRefusesToPushWithNoRemoteHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	root := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")

	st, err := OpenSync(ctx, testEnv().WithRaisePolicy(sourceBuild), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("OpenSync = %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote = %v", err)
	}
	_, err = st.SyncPush(ctx, origin, "master", true, false)
	var refused *SchemaRaiseRequiresReleaseError
	if !errors.As(err, &refused) {
		t.Fatalf("SyncPush with no tracking ref = %v, want *SchemaRaiseRequiresReleaseError", err)
	}
	if refused.RemoteKnown {
		t.Fatalf("refusal = %+v, want the remote's schema unknown", *refused)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err == nil {
		t.Fatal("the remote holds master after the refused push, want no ref written")
	}
}

// A push at the remote's own schema is no raise, so a source build's
// ordinary sync lands: the rule stops raises, not source builds.
func TestASourceBuildPushesAtTheRemotesSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	epic := seedReconcileRemote(t, ctx, producer, remoteURL)
	adoptRemote(t, ctx, consumer, remoteURL)
	createAndPush := func() {
		st, err := Open(ctx, testEnv().WithRaisePolicy(sourceBuild), testStderr(), consumer, "ws")
		if err != nil {
			t.Fatalf("Open(consumer) = %v", err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "from source", Topic: "topic", IssueType: "task", ParentID: epic}); err != nil {
			t.Fatalf("CreateIssue = %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
	}
	createAndPush()

	st, err := OpenSync(ctx, testEnv().WithRaisePolicy(sourceBuild), testStderr(), consumer, "ws")
	if err != nil {
		t.Fatalf("OpenSync(consumer) = %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush at the remote's schema from a source build = %v", err)
	}
	if head := trackingSchema(t, ctx, st); head != knownRemoteSchema(registryMax(t)) {
		t.Fatalf("remote after the push = %+v, want schema %d", head, registryMax(t))
	}
}

// The leased push is the one other DOLT_PUSH, and it judges the raise against
// the head the lease names: a source build's lease over an older head is
// refused before it reaches the remote.
func TestALeasedPushFromASourceBuildNeverRaisesARemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	max := registryMax(t)

	olderHead := seedOlderSchemaRemote(t, ctx, producer, remoteURL)
	adoptAndMigrate(t, ctx, consumer, remoteURL)

	st, err := OpenSync(ctx, testEnv().WithRaisePolicy(sourceBuild), testStderr(), consumer, "ws")
	if err != nil {
		t.Fatalf("OpenSync(consumer) = %v", err)
	}
	defer func() { _ = st.Close() }()
	var outcome storage.LeaseOutcome
	err = st.runSyncMutation(ctx, func(ctx context.Context) error {
		var err error
		outcome, err = st.pushLeasedWithinLock(ctx, remoteName(t, "origin"), "master", "master", olderHead)
		return err
	})
	var refused *SchemaRaiseRequiresReleaseError
	if !errors.As(err, &refused) {
		t.Fatalf("leased push from a source build = %v (outcome %q), want *SchemaRaiseRequiresReleaseError", err, outcome)
	}
	if outcome != "" {
		t.Fatalf("outcome = %q, want none: the push was refused before it reached the remote", outcome)
	}
	want := SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", RemoteSchema: max - 1, RemoteKnown: true, CommitSchema: max, Ceiling: testEnv().ReleasedSchema()}
	if *refused != want {
		t.Fatalf("refusal = %+v, want %+v", *refused, want)
	}
	if head := trackingSchema(t, ctx, st); head != knownRemoteSchema(max-1) {
		t.Fatalf("remote after the refused lease = %+v, want schema %d", head, max-1)
	}
}
