package store

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/conformance"
)

// TestDoltEngineConformance holds this engine to lit's storage contract.
//
// While Dolt is lit's only engine the suite is a regression net: it is written
// from what this engine does, so a failure here means this engine's observable
// behavior moved. Its real job starts with the second engine
// (links-store-seam-q35v.3), where the same statements run against an
// implementation that shares no code with this one — that is the moment the
// suite stops describing Dolt and starts defining storage.
// [LAW:behavior-not-structure]
func TestDoltEngineConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T) storage.Store {
		return openIssueStore(t, context.Background())
	})
}
