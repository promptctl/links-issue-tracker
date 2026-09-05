package memory_test

import (
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/memory"
)

// TestMemoryEngineOffersNoCapability is the mirror of the Dolt engine's
// capability test, and it is a real answer rather than a gap being tolerated.
//
// Every capability names machinery one particular engine happens to have: a
// remote to exchange with, a divergence to settle, a history to check point,
// faults of its own making to repair, a versioned shape to migrate, a native
// language to run a raw statement in. This engine has none of them, so the
// honest answer to all seven is no — and a caller learns that by asking, which
// is what makes absence a value that flows instead of a panic waiting to
// happen. [LAW:no-silent-failure]
func TestMemoryEngineOffersNoCapability(t *testing.T) {
	t.Parallel()
	engine := newEngine(t, storage.SystemClock)

	if offered := storage.Offered(engine); len(offered) != 0 {
		t.Fatalf("the memory engine offers %v, want none", offered)
	}
	// Offered enumerates; Of is what a caller that means to USE a capability
	// asks, and its refusal must name both what was missing and who came up
	// short.
	for _, capability := range storage.Capabilities() {
		if capability.OfferedBy(engine) {
			t.Errorf("%s reports as offered", capability.Name())
		}
	}
	if _, err := storage.Sync.Of(engine); err == nil {
		t.Fatal("Sync.Of returned a syncer; want an UnsupportedError")
	} else {
		var unsupported storage.UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Fatalf("Sync.Of error = %v, want storage.UnsupportedError", err)
		}
		if unsupported.Capability != "sync" {
			t.Errorf("UnsupportedError.Capability = %q, want sync", unsupported.Capability)
		}
		if unsupported.Engine == "" {
			t.Error("UnsupportedError names no engine; the message must say which one came up short")
		}
	}
}

// TestNewRefusesIncompleteConstruction pins both halves an engine cannot come
// into existence without. Either absence surviving construction would surface
// at the first write instead — an unattributed event, or a nil clock call — and
// construction is the one place either can be refused once for every method
// below. [LAW:parse-dont-validate]
//
// The two are one table because they differ in values and not in structure: a
// third precondition is a row here, never another test function.
// [LAW:dataflow-not-control-flow]
func TestNewRefusesIncompleteConstruction(t *testing.T) {
	t.Parallel()
	for _, refusal := range []struct {
		name        string
		workspaceID string
		clock       storage.Clock
	}{
		{"empty workspace id", "", storage.SystemClock},
		{"blank workspace id", "   ", storage.SystemClock},
		{"no clock", "memory-refusals", nil},
	} {
		if _, err := memory.New(refusal.workspaceID, refusal.clock); err == nil {
			t.Errorf("New with %s succeeded; want an error", refusal.name)
		}
	}
}
