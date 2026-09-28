package store

import (
	"context"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/conformance"
)

// TestDoltEngineConformance holds this engine to lit's storage contract.
//
// The suite is written from what this engine does, so a failure here means
// this engine's observable behavior moved. The memory engine runs the same
// statements against an implementation that shares no code with this one,
// which is what makes the suite define storage rather than describe Dolt.
// [LAW:behavior-not-structure]
func TestDoltEngineConformance(t *testing.T) {
	t.Parallel()
	conformance.Run(t, func(t *testing.T, clock storage.Clock) storage.Store {
		st := openIssueStore(t, context.Background())
		// Open() builds every store on the real clock, which is what a person
		// running lit gets; the suite's clock is installed here, in this
		// engine's own package, so the production entry point grows no
		// parameter that only a test would ever vary.
		st.clock = clock
		return st
	})
}

// TestDoltEngineOffersEveryCapability states, at runtime, what contract.go's
// compile-time assertions state statically — but through the discovery
// mechanism a caller actually uses, so the capability values are checked to be
// wired to the interfaces this engine was asserted against.
//
// It is also the shape the second engine's test takes: an engine says which
// capabilities it offers by being asked, and an engine that offers fewer says
// so here rather than anywhere else. [LAW:one-source-of-truth]
func TestDoltEngineOffersEveryCapability(t *testing.T) {
	t.Parallel()
	engine := openIssueStore(t, context.Background())

	offered := []string{}
	for _, capability := range storage.Offered(engine) {
		offered = append(offered, capability.Name())
	}
	want := []string{}
	for _, capability := range storage.Capabilities() {
		want = append(want, capability.Name())
	}
	if !slices.Equal(offered, want) {
		t.Fatalf("the Dolt engine offers %v, want every capability %v", offered, want)
	}
}
