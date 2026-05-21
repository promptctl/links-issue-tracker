// Package migrations holds the goose changeset registry for the links schema.
// 00001_baseline.sql is schema v1 (the converged shape); subsequent migrations
// append as <NNNN>_<name>.sql with strictly ascending versions. Only SQL
// migrations are wired today — both this embed and registryMaxVersion scan
// *.sql; adding a Go migration would require registering it with goose and
// widening the embed pattern.
package migrations

import "embed"

// FS is the embedded goose migration registry. It is the one source the runner
// reads from; nothing applies schema outside this set.
//
//go:embed *.sql
var FS embed.FS
