// Package migrations holds the goose-managed schema changeset registry for the
// store. The embedded filesystem is consumed by the migration runner; goose
// indexes migrations by version prefix in the filename.
package migrations

import "embed"

// FS is the registry that goose reads. Add a new migration by dropping a file
// named `NNNNN_<name>.sql` (or `.go`) into this directory.
//
//go:embed *.sql
var FS embed.FS
