package cli

import "testing"

// Every command that opens the store says how many of the stamps it handed
// out history held back from the clock, and by how much at most — zero
// included, so a command whose clock never ran behind says so rather than
// saying nothing, and a clone whose clock runs behind its peers is read off
// its events rather than inferred from event order.
func TestEachStoreCommandRecordsItsStampCorrections(t *testing.T) {
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	for _, args := range [][]string{{"start", first}, {"ls"}} {
		runCLIInDir(t, dir, args...)
		ev := writtenEvent(t, lastEvent(t))
		if got, ok := ev.Counts["store.stamps_corrected"]; !ok || got != 0 {
			t.Errorf("lit %v event store.stamps_corrected = %v (present %v), want 0 on a clock that never ran behind", args, got, ok)
		}
		if got, ok := ev.Facts["store.stamp_correction_max_ns"]; !ok || got != float64(0) {
			t.Errorf("lit %v event store.stamp_correction_max_ns = %v (present %v), want 0", args, got, ok)
		}
	}
}
