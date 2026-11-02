package store

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// Only a release raises a remote's schema. Let R be the remote head's schema
// and H the schema of the head a push sends: a push with H > R is a raise, and
// it lands only from a release (design-docs/schema-reversibility.md, "What
// rule 2 means, precisely"). A remote whose head this store has never fetched
// has no R, and a push that creates the remote raises it from nothing; both
// count as a raise. The policy enters at the composition root on environ.Env,
// beside the released-schema ceiling, and refuseSchemaRaise below is its only
// reader. [LAW:single-enforcer] How far a release may raise a remote is the
// ceiling's refusal (refuseUnreleasedSchema), which every push runs first.

// remoteSchema is the schema of the remote head a push is judged against, or
// the fact that this store has none to judge against: no tracking ref for the
// branch, because it was never fetched or the push creates it.
// [LAW:types-are-the-program] "unknown" is a variant, not a 0 that would read
// as a pre-goose remote.
type remoteSchema struct {
	version int64
	known   bool
}

// knownRemoteSchema is the remote head's schema, read from its commit.
func knownRemoteSchema(version int64) remoteSchema {
	return remoteSchema{version: version, known: true}
}

// unknownRemoteSchema is a remote this store holds no head of.
func unknownRemoteSchema() remoteSchema {
	return remoteSchema{}
}

// SchemaRaiseRequiresReleaseError refuses a push that would raise a remote's
// schema from a binary that is no release.
type SchemaRaiseRequiresReleaseError struct {
	Remote string
	Branch string
	// RemoteSchema is R, the remote head's schema, meaningful only when
	// RemoteKnown is true.
	RemoteSchema int64
	// RemoteKnown is false when this store holds no head of the remote's
	// branch, so R is unknown and the push counts as a raise.
	RemoteKnown bool
	// CommitSchema is H, the schema of the head the push would send.
	CommitSchema int64
	// Ceiling is this lit's released-schema ceiling, which decides the
	// remedy: a released H is raised by a release's own push, and an
	// unreleased one has only the downgrade.
	Ceiling version.ReleasedSchemaCeiling
}

// Released reports whether a release carries H, so a release's `lit sync
// push` is what raises the remote. Otherwise stepping this store back to R is
// the one remedy: a downgrade of a released schema would only be migrated back
// up by the next open.
func (e *SchemaRaiseRequiresReleaseError) Released() bool {
	return e.CommitSchema <= e.Ceiling.Max
}

func (e *SchemaRaiseRequiresReleaseError) Error() string {
	raise := fmt.Sprintf("the remote's schema is unknown here (%s/%s has no tracking ref), which counts as raising it to %d", e.Remote, e.Branch, e.CommitSchema)
	if e.RemoteKnown {
		raise = fmt.Sprintf("it would raise %s/%s from schema version %d to %d", e.Remote, e.Branch, e.RemoteSchema, e.CommitSchema)
	}
	return fmt.Sprintf("refusing to push to %s/%s: %s, and only a release raises a remote's schema; this lit is built from source, not a release — %s",
		e.Remote, e.Branch, raise, e.remedy())
}

// remedy is the one step that clears the refusal, chosen by the values: fetch
// when R is unknown, a release's push when H is released, the downgrade when
// it is not. [LAW:dataflow-not-control-flow]
func (e *SchemaRaiseRequiresReleaseError) remedy() string {
	switch {
	case !e.RemoteKnown:
		return fmt.Sprintf("fetch first (`lit sync fetch --remote %s`) so the remote's schema is known", e.Remote)
	case e.Released():
		return fmt.Sprintf("schema version %d is released, so a release's `lit sync push` raises the remote", e.CommitSchema)
	default:
		return fmt.Sprintf("schema version %d is above this lit's released-schema ceiling %d, so `lit downgrade` returns this store to schema version %d", e.CommitSchema, e.Ceiling.Max, e.RemoteSchema)
	}
}

// EventRecord is the remote refused, R and whether it was known, H, and the
// ceiling that chose the remedy.
func (e *SchemaRaiseRequiresReleaseError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"schema_raise.remote":         e.Remote,
		"schema_raise.branch":         e.Branch,
		"schema_raise.remote_schema":  e.RemoteSchema,
		"schema_raise.remote_known":   e.RemoteKnown,
		"schema_raise.commit_schema":  e.CommitSchema,
		"schema_raise.ceiling":        e.Ceiling.Max,
		"schema_raise.ceiling_source": e.Ceiling.Source,
	}, nil
}

var _ model.RecordedError = (*SchemaRaiseRequiresReleaseError)(nil)

// refuseSchemaRaise refuses a push of a head at commitSchema to remote/branch
// whose head is at remoteHead, when the push raises the remote and policy is
// no release. A push at or below the remote's schema is no raise and lands
// from any binary. [LAW:effects-at-boundaries] a pure verdict on the values
// the caller read; both push sites run it.
func refuseSchemaRaise(remote storage.RemoteName, branch string, remoteHead remoteSchema, commitSchema int64, policy version.RaisePolicy, ceiling version.ReleasedSchemaCeiling) error {
	raises := !remoteHead.known || commitSchema > remoteHead.version
	if !raises || policy.Release {
		return nil
	}
	return &SchemaRaiseRequiresReleaseError{
		Remote:       remote.String(),
		Branch:       branch,
		RemoteSchema: remoteHead.version,
		RemoteKnown:  remoteHead.known,
		CommitSchema: commitSchema,
		Ceiling:      ceiling,
	}
}
