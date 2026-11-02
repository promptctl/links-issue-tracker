package version

// RaisePolicy is whether this binary may raise a remote's schema: a push
// whose head is at a schema above the remote head's lands only from a
// release. It is built once from the binary's Info at the composition root
// (cmd/lit) and handed down on the store's environ.Env, as the released-schema
// ceiling is; nothing below reads the build stamp. The zero value is no
// release and fails closed: an Env nobody gave a policy raises nothing.
// [LAW:effects-at-boundaries]
//
// How far a release may raise a remote is not a second question here: a
// release's released-schema ceiling is its own registry max
// (ReleasedSchemaCarried), and the ceiling's push refusal already holds every
// push at or below it. [LAW:single-enforcer]
type RaisePolicy struct {
	// Release is whether goreleaser's tagged build produced this binary
	// (Origin == release). A snapshot, an install.sh source build, a `just
	// build` and a bare `go build` all read false.
	Release bool
}

// RaisePolicyOf is the raise policy of the binary info describes.
func RaisePolicyOf(info Info) RaisePolicy {
	return RaisePolicy{Release: !info.FromSource}
}
