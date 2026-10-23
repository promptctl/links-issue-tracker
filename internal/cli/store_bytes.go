package cli

import (
	"context"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// recordStoreBytes puts the size of the store a command ran against on its
// event, so a duration read back later says what it was spent over. It runs
// once the command has closed its store: the reading walks the chunk store,
// and inside the store's lock that walk would lengthen the hold every other
// writer queues behind. [LAW:no-ambient-temporal-coupling] Both keys are set
// on every reading, so a failed one reads as null bytes beside its reason,
// never as a store of 0 bytes. [LAW:no-silent-failure]
func recordStoreBytes(ctx context.Context, databasePath string) {
	telemetry.Record(ctx, storeBytesFacts(engine.ChunkStoreBytes(databasePath)), nil)
}

// runThenMeasure runs a command's work over the store closeStore releases,
// then records that store's size: after the close, so the reading is outside
// the store's lock, and before work's error is returned, because a fact
// recorded before a failure is still true. The one owner of that order for
// every seam that opens the store a command runs against. [LAW:single-enforcer]
func runThenMeasure(ctx context.Context, databasePath string, closeStore func() error, work func() error) error {
	err := func() error {
		defer func() { _ = closeStore() }()
		return work()
	}()
	recordStoreBytes(ctx, databasePath)
	return err
}

// storeBytesFacts is one chunk-store reading as event facts.
func storeBytesFacts(bytes int64, err error) map[string]any {
	var measured *int64
	if err == nil {
		measured = &bytes
	}
	return map[string]any{
		"store.chunk_store_bytes":       measured,
		"store.chunk_store_bytes_error": telemetry.ErrorText(err),
	}
}
