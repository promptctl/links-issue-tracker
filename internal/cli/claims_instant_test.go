package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage/memory"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/promptctl/links-issue-tracker/internal/workspace/workspacetest"
)

// storeEpoch is where the store's clock starts in these tests: decades behind
// wall time, so any claim judged against wall time instead of the store's
// instant reads as long expired.
var storeEpoch = time.Date(2001, 3, 4, 5, 6, 7, 0, time.UTC)

// pinnedStoreHarness is a hold harness over a memory store whose clock reads
// only what the test sets.
func pinnedStoreHarness(t *testing.T) (readyTestHarness, *time.Time) {
	t.Helper()
	setRunEnv(t, "XDG_CONFIG_HOME", t.TempDir())
	setRunEnv(t, "CLAUDE_CODE_SESSION_ID", "")
	ws := workspacetest.Handed(t, runEnv(t), t.TempDir())
	now := storeEpoch
	st, err := memory.New(ws.WorkspaceID, func() time.Time { return now })
	if err != nil {
		t.Fatalf("memory.New() error = %v", err)
	}
	stream, err := workspace.EnsureStream(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.EnsureStream() error = %v", err)
	}
	return readyTestHarness{t: t, ctx: context.Background(), ap: app.New(ws, st, stream)}, &now
}

// A claim line is judged and aged at the store's instant: a ticket started
// half an hour ago on the store's clock reads as claimed, 30 minutes ago,
// though by wall time the start is decades old and long expired.
func TestAClaimLineIsJudgedAndAgedAtTheStoresInstant(t *testing.T) {
	h, now := pinnedStoreHarness(t)
	epic, ticket := h.heldTicket(foreignStream, "session-a")
	*now = storeEpoch.Add(30 * time.Minute)

	cc, err := gatherClaimContext(h.ctx, runEnv(t), io.Discard, h.ap)
	if err != nil {
		t.Fatalf("gatherClaimContext() error = %v", err)
	}
	lane := model.LaneOf(ticket, &epic)
	line, ok := formatClaimLine(cc, lane)
	if !ok {
		t.Fatalf("formatClaimLine(%s) rendered no line; want the claim the store's clock still holds fresh", lane)
	}
	if !strings.Contains(line, "30 minutes ago") {
		t.Fatalf("claim line = %q, want the holder aged 30 minutes by the store's clock", line)
	}
}

// The hold gate judges the hold, and names its holder's age, at the store's
// instant: a phase move from another checkout half an hour after the start on
// the store's clock is refused with the holder active 30 minutes ago, though
// by wall time the hold expired decades ago and would not refuse at all.
func TestTheHoldGateJudgesAndAgesAtTheStoresInstant(t *testing.T) {
	h, now := pinnedStoreHarness(t)
	_, ticket := h.heldTicket(foreignStream, "session-a")
	*now = storeEpoch.Add(30 * time.Minute)

	_, err := h.runTransitionAs(phaseSpec, ticket.ID, "session-b", "--to", string(model.PhaseReview))
	holder := nameCheckout(model.NewAttribution(foreignStream, h.ap.Workspace.WorkspaceID))
	assertRefused(t, err, ticket.ID.Raw()+" is held by "+holder+" (active 30 minutes ago)"+actsOnHeld.refusal)
}

// A run judges at the instant the store would stamp a write with, so a store
// clock that lags its own history judges at the history instead: activity the
// history records reads as a moment ago, never as ahead of the run.
func TestAClaimIsJudgedNoEarlierThanTheHistoryItRead(t *testing.T) {
	h, now := pinnedStoreHarness(t)
	epic, ticket := h.heldTicket(foreignStream, "session-a")
	*now = storeEpoch.Add(-time.Hour)

	cc, err := gatherClaimContext(h.ctx, runEnv(t), io.Discard, h.ap)
	if err != nil {
		t.Fatalf("gatherClaimContext() error = %v", err)
	}
	if !cc.judgedAt.After(storeEpoch) {
		t.Fatalf("judged at %s, want past the start recorded at %s", cc.judgedAt, storeEpoch)
	}
	line, ok := formatClaimLine(cc, model.LaneOf(ticket, &epic))
	if !ok || !strings.Contains(line, "under a minute ago") {
		t.Fatalf("claim line = (%q, %v), want the holder active under a minute ago", line, ok)
	}
}
