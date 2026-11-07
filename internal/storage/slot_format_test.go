package storage_test

import (
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestSlotFormatsAsOneHeldValue pins that every slot kind holds each of its
// parts to the row under %v and shows their raw escapes under %q.
func TestSlotFormatsAsOneHeldValue(t *testing.T) {
	t.Parallel()
	issue := model.ID("a\nb")
	for _, tc := range []struct {
		slot      storage.Slot
		held, raw string
	}{
		{storage.Slot{Kind: storage.FieldSlot, Issue: issue, Name: "t\tx"}, "a" + model.QuotedBreak + "b t x", `"a\nb t\tx"`},
		{storage.Slot{Kind: storage.ParentSlot, Issue: issue}, "a" + model.QuotedBreak + "b parent", `"a\nb parent"`},
		{storage.Slot{Kind: storage.BlockedBySlot, Issue: issue, Name: "c\nd"}, "a" + model.QuotedBreak + "b blocked-by c" + model.QuotedBreak + "d", `"a\nb blocked-by c\nd"`},
	} {
		if got := fmt.Sprintf("%v", tc.slot); got != tc.held {
			t.Errorf("%%v of kind %d = %q, want %q", tc.slot.Kind, got, tc.held)
		}
		if got := fmt.Sprintf("%q", tc.slot); got != tc.raw {
			t.Errorf("%%q of kind %d = %q, want %q", tc.slot.Kind, got, tc.raw)
		}
	}
}
