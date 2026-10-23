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
