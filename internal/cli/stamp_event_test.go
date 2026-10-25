package cli

import (
	"reflect"
	"testing"
)

// Every command that opens the store says, once per store, how many of its
// clock readings history held back, and by how much at most — zero
// included, so a command whose clock never ran behind says so rather than
// saying nothing, and a clone whose clock runs behind its peers is read off
// its events rather than inferred from event order.
func TestEachStoreCommandRecordsItsStampCorrections(t *testing.T) {
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	for _, args := range [][]string{{"start", first}, {"ls"}} {
		runCLIInDir(t, dir, args...)
		ev := writtenEvent(t, lastEvent(t))
		got := ev.Occurrences["store.stamp_corrections"]
		want := []any{map[string]any{"store.stamps_corrected": float64(0), "store.stamp_correction_max_ns": float64(0)}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event store.stamp_corrections = %v, want %v: one store, a clock that never ran behind", args, got, want)
		}
	}
}
