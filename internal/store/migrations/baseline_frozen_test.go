package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// baselineFrozenHash pins the exact bytes of 00001_baseline.sql. The hash is
// the single enforcer of the frozen-file discipline that the header comment
// on 00001_baseline.sql declares; if the bytes drift, this test fails loudly
// and the failure message tells the reviewer to open a new migration file
// instead of "fixing" the test by bumping the constant.
//
// Active since PR #145 (commit 8bc1e8e). lit has not tagged v0.1.0 yet, but
// real workspaces (unreal-3d-maps, cc-nerf-buster) already exist on disk;
// every retcon of baseline.sql between now and v0.1.0 would re-brick them by
// the same mechanism PR #143 / PR #145 just recovered from. Master is treated
// as the immutable baseline from this commit forward.
//
// [LAW:single-enforcer] One enforcer, not two. Reviewer attention and
// documentation discipline both failed for the 2026-05-21 retcon incident;
// this test is the only thing standing between that incident and its repeat.
// [LAW:one-source-of-truth] The hash IS the schema-v1 identity. Two copies
// (one here, one in some workflow yaml) would drift; one copy in Go, run
// from the same code path in CI and `go test ./...`, cannot.
// [LAW:types-are-the-program] Pinning the bytes encodes the strongest true
// theorem about the file — "this is exactly what shipped" — at the type
// level. Anything weaker (schema parser, table-set check) accepts edits
// that change the registry's meaning while keeping the parsed shape equal.
const baselineFrozenHash = "ad9f3c695adba92b0bb71e345d6722b84c060347dd1a84a44b9997836b46a397"

// TestBaselineFileIsFrozen asserts the bytes of 00001_baseline.sql match the
// pinned hash. If this fails, you are about to ship the 2026-05-21 retcon
// incident again — read the failure message before reaching for the constant.
func TestBaselineFileIsFrozen(t *testing.T) {
	data, err := FS.ReadFile("00001_baseline.sql")
	if err != nil {
		t.Fatalf("read embedded 00001_baseline.sql: %v", err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got == baselineFrozenHash {
		return
	}
	t.Fatalf(`00001_baseline.sql is FROZEN and its bytes changed.
  want sha256: %s
  got  sha256: %s

This file is the immutable definition of schema v1. The bytes ARE the meaning
of "v1" for every binary that embeds them; editing it after release silently
changes that meaning and bricks every workspace last-touched before the edit
ships (PR #143 / PR #145 recovered from exactly this — do not re-create the
incident).

If you intend a SCHEMA CHANGE:
  Revert 00001_baseline.sql and add 00002_<your-change>.sql (or the next free
  number). Goose will apply your migration on top of v1 for fresh workspaces
  and stamp it for existing ones.

If you intend a NON-STRUCTURAL EDIT (comment, whitespace, typo):
  You still cannot edit this file. The gate cannot distinguish "harmless"
  edits from structural ones — that distinction is exactly what failed in
  the original incident. Put the comment elsewhere (a sibling .md, the
  package doc in embed.go, or schema_reconcile.go), or open a numbered
  migration that does nothing but document the clarification.

DO NOT update baselineFrozenHash to match the new bytes. Doing so silently
re-enables the bug class this gate exists to prevent.`,
		baselineFrozenHash, got)
}
