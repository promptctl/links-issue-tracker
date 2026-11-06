package store

import (
	"context"
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
// ceiling's refusal (refuseSchemaAboveCeiling), which runs first on every
// raise. A push with H <= R is no raise: it is judged against R alone, never
// against this binary (design-docs/schema-reversibility.md, "A push at the
// remote's own schema lands").

// remoteSchema is the schema of the remote head a push is judged against and
// the schema hold that head carries, or the fact that this store has none to
// judge against: no tracking ref for the branch, because it was never fetched
// or the push creates it. [LAW:types-are-the-program] "unknown" is a variant,
// not a 0 that would read as a pre-goose remote; a head with no hold carries
// the unbounded ceiling, which lowers nothing.
type remoteSchema struct {
	version int64
	known   bool
	// head is the commit the schema and hold were read AS OF, "" when the
	// remote is unknown. A verdict on this value is a verdict on that commit,
	// so a replay onto head replays onto the head the guard judged.
	// [LAW:parse-dont-validate] the proof names what it proves.
	head string
	// hold is the schema hold recorded at the head (schema_hold.go): the
	// ceiling a push to this remote may not send a commit above.
	hold version.SchemaCeiling
}

// knownRemoteSchema is a remote head at schema with no schema hold.
func knownRemoteSchema(schema int64) remoteSchema {
	return remoteSchema{version: schema, known: true, hold: version.UnboundedSchemaCeiling()}
}

// unknownRemoteSchema is a remote this store holds no head of.
func unknownRemoteSchema() remoteSchema {
	return remoteSchema{hold: version.UnboundedSchemaCeiling()}
}

// raisedBy reports whether a push of a head at commitSchema raises this
// remote: H > R, or R unknown, since a push that creates the remote raises it
// from nothing. The ceiling's refusal and the raise policy's both judge a
// raise by this one predicate. [LAW:one-source-of-truth]
func (r remoteSchema) raisedBy(commitSchema int64) bool {
	return !r.known || commitSchema > r.version
}

// remoteHeadAt is the remote head at commitHash as a push judges it: its
// schema and its hold, both read AS OF that one commit, so the two cannot
// come from different heads. The hold's source names the head as
// "<remote>/<branch>'s head". [LAW:one-source-of-truth]
func (s *Store) remoteHeadAt(ctx context.Context, commitHash string, remote storage.RemoteName, branch string) (remoteSchema, error) {
	remoteVersion, err := s.remoteHeadSchema(ctx, commitHash)
	if err != nil {
		return remoteSchema{}, err
	}
	hold, err := s.schemaHoldAt(ctx, commitHash, fmt.Sprintf("%s/%s's head", remote, branch))
	if err != nil {
		return remoteSchema{}, err
	}
	return remoteSchema{version: remoteVersion, known: true, hold: hold, head: commitHash}, nil
}

// SchemaRaiseRequiresReleaseError refuses a push that would raise a remote's
// schema from a binary that is no release. H is at or below this lit's
// released-schema ceiling: the ceiling's refusal runs before this one, so a
// schema no release carries never reaches it.
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
}

func (e *SchemaRaiseRequiresReleaseError) Error() string {
	raise := fmt.Sprintf("the remote's schema is unknown here (%s/%s has no tracking ref), which counts as raising it to %d", e.Remote, e.Branch, e.CommitSchema)
	if e.RemoteKnown {
		raise = fmt.Sprintf("it would raise %s/%s from schema version %d to %d", e.Remote, e.Branch, e.RemoteSchema, e.CommitSchema)
	}
	return fmt.Sprintf("refusing to push to %s/%s: %s, and only a release raises a remote's schema; this lit is not a release — %s",
		e.Remote, e.Branch, raise, e.remedy())
}

// remedy is what clears the refusal. R is the tracking ref's head as of the
// last fetch, so a fetch comes first: a release may have raised the remote
// since, and a remote never fetched may already be at H. A push still refused
// after the fetch is a raise only a release makes.
func (e *SchemaRaiseRequiresReleaseError) remedy() string {
	return fmt.Sprintf("fetch first (`lit sync fetch --remote %s`) so the remote's head is current here; a push at or below it lands from this lit, and a push still refused after the fetch waits for a release's `lit sync push` to raise the remote to schema version %d", e.Remote, e.CommitSchema)
}

// EventRecord is the remote refused, H, and R when it was known: an unknown R
// has no number, and a 0 would read as a pre-goose remote.
func (e *SchemaRaiseRequiresReleaseError) EventRecord() (map[string]any, map[string]int) {
	fields := map[string]any{
		"schema_raise.remote":        e.Remote,
		"schema_raise.branch":        e.Branch,
		"schema_raise.remote_known":  e.RemoteKnown,
		"schema_raise.commit_schema": e.CommitSchema,
	}
	if e.RemoteKnown {
		fields["schema_raise.remote_schema"] = e.RemoteSchema
	}
	return fields, nil
}

var _ model.RecordedError = (*SchemaRaiseRequiresReleaseError)(nil)

// refuseSchemaRaise refuses a push of a head at commitSchema to remote/branch
// whose head is at remoteHead, when the push raises the remote and policy is
// no release. A push at or below the remote's schema is no raise and lands
// from any binary. [LAW:effects-at-boundaries] a pure verdict on the values
// the caller read; both push sites run it.
func refuseSchemaRaise(remote storage.RemoteName, branch string, remoteHead remoteSchema, commitSchema int64, policy version.RaisePolicy) error {
	if !remoteHead.raisedBy(commitSchema) || policy.Release {
		return nil
	}
	return &SchemaRaiseRequiresReleaseError{
		Remote:       remote.String(),
		Branch:       branch,
		RemoteSchema: remoteHead.version,
		RemoteKnown:  remoteHead.known,
		CommitSchema: commitSchema,
	}
}
