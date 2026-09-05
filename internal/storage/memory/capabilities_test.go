package memory_test

import (
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
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

// TestNewRequiresWorkspaceID pins the one thing an engine cannot be built
// without. Attribution is a complete pair or nothing, so an engine with no
// workspace to scope its stream token could only record unattributed events —
// and it would do so silently, which is the failure this refusal deletes.
func TestNewRequiresWorkspaceID(t *testing.T) {
	t.Parallel()
	for _, workspaceID := range []string{"", "   "} {
		if _, err := newEngineWithWorkspace(workspaceID); err == nil {
			t.Errorf("New(%q) succeeded; want an error", workspaceID)
		}
	}
}
