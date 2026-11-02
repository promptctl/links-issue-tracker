package version

// RaisePolicy is whether this binary may raise a remote's schema: a push
// whose head is at a schema above the remote head's lands only from a
// release. It is built once from the Origin stamp at the composition root
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

// IsRelease reports whether an Origin stamp names goreleaser's tagged build:
// the one definition of "this binary is a release" that Info.FromSource and
// the raise policy both read. [LAW:one-source-of-truth]
func IsRelease(origin string) bool {
	return origin == OriginRelease
}

// RaisePolicyOf is the raise policy of a binary whose Origin stamp is origin.
// It reads the stamp alone, no registry: the composition root builds it
// beside the ceiling without a second ReadDir.
func RaisePolicyOf(origin string) RaisePolicy {
	return RaisePolicy{Release: IsRelease(origin)}
}
