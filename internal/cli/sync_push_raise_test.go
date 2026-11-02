package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// runSourceBuildInDir runs the real CLI as a binary built from source would:
// the invocation's Env carries the zero raise policy cmd/lit builds for every
// Origin but release. Everything else is testEnv's.
func runSourceBuildInDir(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	inv := dirInvocation(dir, &out, &out, args)
	inv.Env = testEnv().WithRaisePolicy(version.RaisePolicy{})
	return out.String(), Run(context.Background(), inv)
}

// asSyncFailure routes the store's schema-raise refusal into the sync-failure
// contract: a block naming both schemas and the remedy the refusal's values
// choose.
func TestASchemaRaiseRefusalRendersTheSyncFailureContract(t *testing.T) {
	t.Parallel()
	storeErr := &store.SchemaRaiseRequiresReleaseError{
		Remote: "origin", Branch: "master", RemoteSchema: 11, RemoteKnown: true, CommitSchema: 12,
		Ceiling: version.ReleasedSchemaCeiling{Max: 12, Source: "stamped at build time from release tag v0.24.0"},
	}
	var syncFailure SyncFailureError
	if !errors.As(asSyncFailure(storeErr), &syncFailure) {
		t.Fatalf("asSyncFailure(%v) is not a SyncFailureError", storeErr)
	}
	if syncFailure.Failure.Class != syncFailureSchemaRaiseRequiresRelease || syncFailure.Failure.RaiseRefused != storeErr {
		t.Fatalf("mapped failure = %+v", syncFailure.Failure)
	}
	block := syncFailure.Error()
	for _, want := range []string{
		"would raise the remote's schema and this lit is not a release",
		"is at schema version 12, and origin/master is at schema version 11",
		"raises the remote to schema v12",
		"ESCALATION — BLOCKED",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}

	unreleased := &store.SchemaRaiseRequiresReleaseError{
		Remote: "origin", Branch: "master", RemoteSchema: 11, RemoteKnown: true, CommitSchema: 12,
		Ceiling: version.ReleasedSchemaCeiling{Max: 11, Source: "the checkout this lit was built from had no release tag"},
	}
	if block := asSyncFailure(unreleased).Error(); !strings.Contains(block, "returns this backlog to schema v11") {
		t.Errorf("block for an unreleased schema lacks the downgrade remedy:\n%s", block)
	}
	unknown := &store.SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", CommitSchema: 12, Ceiling: version.ReleasedSchemaCeiling{Max: 12}}
	if block := asSyncFailure(unknown).Error(); !strings.Contains(block, "lit sync fetch --remote origin") {
		t.Errorf("block for an unknown remote schema lacks the fetch remedy:\n%s", block)
	}
}

// A binary built from source syncs as before: `lit init` adopts the remote's
// head, so its push sends a backlog at the remote's own schema, which is no
// raise. The rule stops raises, not source builds, and a raise never comes
// out of this flow, so the store's tests are where the refusal is proven.
func TestASourceBuildPushesAtTheRemotesSchema(t *testing.T) {
	remote, _, consumer, _, epicID := twoClonesOverOneDoltRemote(t)
	before := remoteDoltHead(t, remote)
	runCLIInDir(t, consumer, "new", "--title", "from-source", "--topic", "demo", "--type", "task", "--parent", epicID)

	if out, err := runSourceBuildInDir(t, consumer, "sync", "push"); err != nil {
		t.Fatalf("source build's push at the remote's schema error = %v\noutput:\n%s", err, out)
	}
	if after := remoteDoltHead(t, remote); after == before {
		t.Fatalf("remote head after the source build's push = %s, unchanged", after)
	}
}
