package memory_test

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/conformance"
	"github.com/promptctl/links-issue-tracker/internal/storage/memory"
)

// TestConformance is this engine's whole claim to implement the contract. It
// is the same suite the Dolt engine runs, so a statement that passes here and
// there is a statement about the contract rather than about either engine.
func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T, clock storage.Clock) storage.Store { return newEngine(t, clock) })
}

// newEngine mints a fresh engine for one case. Every case gets its own: a
// suite whose cases shared a store would be pinning the order they run in as
// much as the contract.
func newEngine(t *testing.T, clock storage.Clock) *memory.Engine {
	t.Helper()
	engine, err := memory.New("memory-conformance", clock)
	if err != nil {
		t.Fatalf("memory.New error = %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("Close error = %v", err)
		}
	})
	return engine
}

// newEngineWithWorkspace is the bare constructor call, kept separate so the
// construction refusals can be tested without a t.Cleanup that would never run.
func newEngineWithWorkspace(workspaceID string) (*memory.Engine, error) {
	return memory.New(workspaceID, storage.SystemClock)
}
