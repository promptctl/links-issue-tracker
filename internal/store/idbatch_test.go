package store

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// idBatches must partition its input: every id exactly once, in order, in
// batches no larger than the cap.
//
// The sizes are chosen around the boundary rather than sampled, because every
// off-by-one this helper can carry lives there: one short of a full batch, one
// over, an exact multiple, and a partial batch trailing two full ones.
func TestIDBatchesPartitionTheInput(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, idBatchSize - 1, idBatchSize, idBatchSize + 1, 2*idBatchSize + 2} {
		t.Run(fmt.Sprintf("%d", size), func(t *testing.T) {
			t.Parallel()
			ids := make([]string, size)
			for i := range ids {
				ids[i] = fmt.Sprintf("id-%03d", i)
			}

			var flat []string
			for _, batch := range idBatches(ids) {
				if len(batch) == 0 {
					t.Fatalf("idBatches(%d ids) produced an empty batch, which would build `IN ()`", size)
				}
				if len(batch) > idBatchSize {
					t.Fatalf("idBatches(%d ids) produced a batch of %d, over the cap of %d", size, len(batch), idBatchSize)
				}
				flat = append(flat, batch...)
			}
			if !slices.Equal(flat, ids) {
				t.Fatalf("idBatches(%d ids) concatenated to %v, want the input unchanged and in order", size, flat)
			}
		})
	}
}

// idBatches drops repeats, because the `IN` clause it replaces did.
//
// A repeated id is harmless in one clause and not in several: the two copies
// land in different batches, each batch answers, and the caller sees the row
// twice. The repeats here are placed to straddle a boundary, since a pair
// inside one batch would be collapsed by the query itself and would pass
// whether or not idBatches deduped anything.
func TestIDBatchesDropsRepeatsAcrossBatchBoundaries(t *testing.T) {
	t.Parallel()

	ids := make([]string, 0, 2*idBatchSize)
	for i := range idBatchSize + 2 {
		ids = append(ids, fmt.Sprintf("id-%03d", i))
	}
	// Re-state the first and the last, far enough apart to fall in different
	// batches than their originals.
	ids = append(ids, "id-000", fmt.Sprintf("id-%03d", idBatchSize+1))

	var flat []string
	for _, batch := range idBatches(ids) {
		if len(batch) > idBatchSize {
			t.Fatalf("batch of %d, over the cap of %d", len(batch), idBatchSize)
		}
		flat = append(flat, batch...)
	}

	seen := map[string]int{}
	for _, id := range flat {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("id %s appears %d times across the batches, want once — the caller would read its rows %d times", id, n, n)
		}
	}
	if len(flat) != idBatchSize+2 {
		t.Fatalf("batches carry %d ids, want the %d distinct ones", len(flat), idBatchSize+2)
	}
	// Compared against the order the ids first appear in, not against sorted
	// order. The fixture is generated ascending, so the two coincide here and a
	// sortedness check would pass an idBatches that sorted its output -- which
	// is the one plausible way the documented order could actually break.
	seenOrder := make([]string, 0, len(flat))
	already := map[string]bool{}
	for _, id := range ids {
		if !already[id] {
			already[id] = true
			seenOrder = append(seenOrder, id)
		}
	}
	if !slices.Equal(flat, seenOrder) {
		t.Fatalf("batches are %v, want %v — first-appearance order", flat, seenOrder)
	}
}

// An id set larger than one batch must come back whole.
//
// This is the failure the batching can hide: a read that queries only the first
// batch, or drops the trailing partial one, returns a result that is a union of
// whatever it did ask for — and at every call site a subject whose rows were
// never queried is indistinguishable from a subject that genuinely has no rows.
// Nothing errors, and the caller prints a shorter truth.
//
// The three wired subjects sit in the first, a middle, and the final partial
// batch of the id slice as it is passed, so the assertion fails if any batch
// after the first is skipped, and fails if the last short one is dropped. Both
// endpoint queries are covered, on those same subjects.
func TestGetRelationsByIDsSpansEveryBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const subjectCount = 2*idBatchSize + 2
	subjects := make([]string, 0, subjectCount)
	for i := range subjectCount {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("Subject %d", i), Topic: "batch",
			IssueType: "task", Placement: storage.RankBottom,
		})
		if err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		subjects = append(subjects, issue.ID)
	}
	// The two endpoint queries need edges that only one of them can see. Every
	// edge here runs between a queried subject and a peer that is never queried,
	// so a subject's edge to sink is reachable only through the src_id query and
	// its edge from source only through the dst_id query. Hanging both on the
	// same wired subjects checks each query across the first, a middle and the
	// trailing partial batch at once.
	//
	// Wiring both directions to one shared peer instead would cover neither: if
	// that peer were itself queried, whichever query reached it would return the
	// whole edge set and answer for the other, and the first truncation aimed at
	// either would pass.
	sink, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Sink", Topic: "batch", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(sink) error = %v", err)
	}
	source, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Source", Topic: "batch", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(source) error = %v", err)
	}

	// One subject per batch the slice spans: the first, one inside the second,
	// and the last, which is in the trailing partial batch.
	wired := []int{0, idBatchSize + 1, subjectCount - 1}
	for _, i := range wired {
		mustRelate(t, ctx, st, subjects[i], sink.ID, "blocks")
		mustRelate(t, ctx, st, source.ID, subjects[i], "blocks")
	}

	rels, err := st.GetRelationsByIDs(ctx, subjects)
	if err != nil {
		t.Fatalf("GetRelationsByIDs(%d ids) error = %v", len(subjects), err)
	}

	for _, i := range wired {
		id := subjects[i]
		got, ok := rels[id]
		if !ok {
			t.Fatalf("subject %d of %d (%s) is missing from the result: its batch was never queried", i, subjectCount, id)
		}
		if len(got.DependsOn) != 1 || got.DependsOn[0].ID != sink.ID {
			t.Fatalf("subject %d of %d (%s) DependsOn = %v, want [%s] — a src_id batch went unqueried", i, subjectCount, id, ids(got.DependsOn), sink.ID)
		}
		if len(got.Blocks) != 1 || got.Blocks[0].ID != source.ID {
			t.Fatalf("subject %d of %d (%s) Blocks = %v, want [%s] — a dst_id batch went unqueried", i, subjectCount, id, ids(got.Blocks), source.ID)
		}
	}
}
