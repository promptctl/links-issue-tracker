package store

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// A remote is lowered only by a head that holds itself there. Let R be the
// remote head's schema and H the schema of the head a push sends: a push with
// H < R is a lowering, and it lands only when the head carries a schema hold
// (schema_hold.go), which `lit downgrade --remote` alone writes
// (design-docs/schema-reversibility.md, "Lowering a remote, and making it stay
// lowered"). An unheld lowering is a remote every vN client re-raises on its
// next sync, the state the hold exists to prevent; and it is what a lift
// pushes when the lowering before it never landed, so the lift's failure mode
// is refused here rather than reported as a lift. [LAW:single-enforcer] the
// one verdict on a lowering, beside the ceiling's and the raise's in
// refuseSchemaSend.

// SchemaLoweringRequiresHoldError refuses a push that would lower a remote's
// schema with a head that carries no schema hold.
type SchemaLoweringRequiresHoldError struct {
	Remote string
	Branch string
	// RemoteSchema is R, the remote head's schema as of the last fetch.
	RemoteSchema int64
	// CommitSchema is H, the schema of the head the push would send.
	CommitSchema int64
}

func (e *SchemaLoweringRequiresHoldError) Error() string {
	return fmt.Sprintf("refusing to push to %s/%s: it would lower %s/%s from schema version %d to %d with a head that holds no schema hold, and a remote is lowered only by `lit downgrade --schema %d --remote %s`, whose head holds the remote there",
		e.Remote, e.Branch, e.Remote, e.Branch, e.RemoteSchema, e.CommitSchema, e.CommitSchema, e.Remote)
}

// EventRecord is the remote refused and both schemas.
func (e *SchemaLoweringRequiresHoldError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
		"schema_lowering.remote":        e.Remote,
		"schema_lowering.branch":        e.Branch,
		"schema_lowering.remote_schema": e.RemoteSchema,
		"schema_lowering.commit_schema": e.CommitSchema,
	}, nil
}

var _ model.RecordedError = (*SchemaLoweringRequiresHoldError)(nil)

// refuseSchemaLowering refuses a push of an unheld head at commitSchema to
// remote/branch whose head is at remoteHead, when the push lowers the remote.
// A push at or above the remote's schema is no lowering; a remote this store
// holds no head of is not lowered; a held head lowers the remote on purpose.
// [LAW:effects-at-boundaries] a pure verdict on the values the caller read.
func refuseSchemaLowering(remote storage.RemoteName, branch string, remoteHead remoteSchema, commitSchema int64, commitHold version.SchemaCeiling) error {
	lowers := remoteHead.known && commitSchema < remoteHead.version
	if !lowers || commitHold.Bound == version.BoundHold {
		return nil
	}
	return &SchemaLoweringRequiresHoldError{
		Remote:       remote.String(),
		Branch:       branch,
		RemoteSchema: remoteHead.version,
		CommitSchema: commitSchema,
	}
}
