// Package migrations holds the goose changeset registry for the links schema.
// 00001_baseline.sql is schema v1 (the converged shape); subsequent migrations
// append as <NNNNN>_<name>.sql with strictly ascending versions. Only SQL
// migrations are wired today — both this embed and ParseRegistry read only
// *.sql; adding a Go migration would require registering it with goose and
// widening the embed pattern.
package migrations

import "embed"

// FS is the embedded goose migration registry. It is the one source the runner
// reads from; nothing applies schema outside this set.
//
//go:embed *.sql
var FS embed.FS

// StashCarrierDDL is the one definition of the stash carrier, schema_stash
// (README.md, "The stash carrier"). Every step that stashes or restores runs
// it first, byte for byte; TestEveryStashCarrierCopyIsTheCanonicalDDL holds
// every copy in the registry to it. [LAW:one-source-of-truth]
//
//go:embed stash_carrier.ddl
var StashCarrierDDL string
