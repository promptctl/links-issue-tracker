// Package migrations holds the goose-managed schema changeset registry for the
// store. The embedded filesystem is consumed by the migration runner; goose
// indexes migrations by version prefix in the filename.
package migrations

import "embed"

// FS is the registry that goose reads. Add a new SQL migration by dropping
// a file named `NNNNN_<name>.sql` into this directory — the embed below
// picks it up automatically. Go migrations are not loaded by this FS; they
// must be registered programmatically via `goose.WithGoMigrations` on the
// provider in `migration_runner.go`.
//
//go:embed *.sql
var FS embed.FS
