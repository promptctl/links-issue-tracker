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
// after the first is skipped, and fails if the last short one is dropped.
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
	upstream, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Upstream", Topic: "batch", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(upstream) error = %v", err)
	}

	// One subject per batch the slice spans: the first, one inside the second,
	// and the last, which is in the trailing partial batch.
	wired := []int{0, idBatchSize + 1, subjectCount - 1}
	for _, i := range wired {
		mustRelate(t, ctx, st, subjects[i], upstream.ID, "blocks")
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
		if len(got.DependsOn) != 1 || got.DependsOn[0].ID != upstream.ID {
			t.Fatalf("subject %d of %d (%s) DependsOn = %v, want [%s]", i, subjectCount, id, ids(got.DependsOn), upstream.ID)
		}
	}

	// The reverse edge answers the same question from the other side: upstream
	// is blocked-by every wired subject, and that bundle is assembled from the
	// dst_id query, whose id list is batched the same way.
	blocks := rels[subjects[0]].Blocks
	if len(blocks) != 0 {
		t.Fatalf("subject 0 Blocks = %v, want none — it depends on upstream rather than blocking it", ids(blocks))
	}
}
