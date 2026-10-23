package store

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// idChunks must partition its input: every id exactly once, in order, in
// chunks no larger than the cap.
//
// The sizes are chosen around the boundary rather than sampled, because every
// off-by-one this helper can carry lives there: one short of a full chunk, one
// over, an exact multiple, and a partial chunk trailing two full ones.
func TestIDChunksPartitionTheInput(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, idChunkSize - 1, idChunkSize, idChunkSize + 1, 2*idChunkSize + 2} {
		t.Run(fmt.Sprintf("%d", size), func(t *testing.T) {
			t.Parallel()
			ids := make([]string, size)
			for i := range ids {
				ids[i] = fmt.Sprintf("id-%03d", i)
			}

			var flat []string
			for _, chunk := range idChunks(ids) {
				if len(chunk) == 0 {
					t.Fatalf("idChunks(%d ids) produced an empty chunk, which would build `IN ()`", size)
				}
				if len(chunk) > idChunkSize {
					t.Fatalf("idChunks(%d ids) produced a chunk of %d, over the cap of %d", size, len(chunk), idChunkSize)
				}
				flat = append(flat, chunk...)
			}
			if !slices.Equal(flat, ids) {
				t.Fatalf("idChunks(%d ids) concatenated to %v, want the input unchanged and in order", size, flat)
			}
		})
	}
}

// idChunks drops repeats, keeping first-appearance order.
//
// A repeated id is harmless in one clause and not in several: the two copies
// land in different chunks and the write names the row twice. The repeats are
// placed to straddle a boundary, since a pair inside one chunk would be
// collapsed by the statement itself and would pass whether or not idChunks
// deduped anything.
func TestIDChunksDropRepeatsAcrossChunkBoundaries(t *testing.T) {
	t.Parallel()

	ids := make([]string, 0, 2*idChunkSize)
	for i := range idChunkSize + 2 {
		ids = append(ids, fmt.Sprintf("id-%03d", i))
	}
	// Re-state the first and the last, far enough apart to fall in different
	// chunks than their originals.
	ids = append(ids, "id-000", fmt.Sprintf("id-%03d", idChunkSize+1))

	var flat []string
	for _, chunk := range idChunks(ids) {
		flat = append(flat, chunk...)
	}
	// Compared against the order the ids first appear in, not against sorted
	// order. The fixture is generated ascending, so the two coincide here and a
	// sortedness check would pass an idChunks that sorted its output.
	if want := ids[:idChunkSize+2]; !slices.Equal(flat, want) {
		t.Fatalf("chunks are %v, want %v — each id once, in first-appearance order", flat, want)
	}
}

// A read over an idSet answers each row once however often the caller names
// its id, and answers an empty set with no rows rather than an error.
//
// Both are properties the join has to be given: a join against a set that
// held an id twice would return that id's rows twice, and nothing downstream
// would notice — labels accumulate into a map and would list a label twice.
// And an empty set is the one an `IN ()` clause could not express at all.
func TestIDSetReadsAnswerEachRowOnceAndTheEmptySetWithNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Labelled", Topic: "idset", IssueType: "epic", Labels: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	got, err := st.loadLabelsByIssueIDs(ctx, []string{issue.ID, issue.ID, issue.ID})
	if err != nil {
		t.Fatalf("loadLabelsByIssueIDs(one id three times) error = %v", err)
	}
	if want := []string{"alpha", "beta"}; !slices.Equal(got[issue.ID], want) {
		t.Fatalf("labels of an id named three times = %v, want %v", got[issue.ID], want)
	}

	empty, err := st.loadLabelsByIssueIDs(ctx, nil)
	if err != nil {
		t.Fatalf("loadLabelsByIssueIDs(no ids) error = %v, want no rows", err)
	}
	if len(empty) != 0 {
		t.Fatalf("loadLabelsByIssueIDs(no ids) = %v, want no rows", empty)
	}
}

// A large id set must come back whole.
//
// A read that answers for only part of its ids returns a result that is still
// well-formed — and at every call site a subject whose rows were never read is
// indistinguishable from a subject that genuinely has no rows. Nothing errors,
// and the caller prints a shorter truth.
//
// The three wired subjects sit at the start, the middle, and the end of the id
// slice as it is passed. Both endpoint queries are covered, on those same
// subjects.
func TestGetRelationsByIDsAnswersForEveryID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	fx := newBatchedRelationFixture(t, ctx, st)

	wired := []int{0, len(fx.subjects) / 2, len(fx.subjects) - 1}
	fx.wire(t, ctx, st, wired)

	rels, err := st.GetRelationsByIDs(ctx, fx.subjects)
	if err != nil {
		t.Fatalf("GetRelationsByIDs(%d ids) error = %v", len(fx.subjects), err)
	}

	for _, i := range wired {
		id := fx.subjects[i]
		got, ok := rels[id]
		if !ok {
			t.Fatalf("subject %d of %d (%s) is missing from the result: it was never read", i, len(fx.subjects), id)
		}
		if len(got.BlockedBy) != 1 || got.BlockedBy[0].ID != fx.sink.ID {
			t.Fatalf("subject %d of %d (%s) BlockedBy = %v, want [%s] — the src_id read missed it", i, len(fx.subjects), id, ids(got.BlockedBy), fx.sink.ID)
		}
		if len(got.Blocks) != 1 || got.Blocks[0].ID != fx.source.ID {
			t.Fatalf("subject %d of %d (%s) Blocks = %v, want [%s] — the dst_id read missed it", i, len(fx.subjects), id, ids(got.Blocks), fx.source.ID)
		}
	}
}

// A read answers from the moment its store opened even while another engine
// writes, because an engine reads the root it loaded at open and never reloads
// it — see ONE ROOT PER ENGINE in the package doc. GetRelationsByIDs is two
// statements, one per endpoint column, and agrees with itself only while that
// holds, so this pins it at the contract.
//
// Edges wired before the reader opens must all reach it, and edges wired after
// must not. The two sets sit on different subjects at the start, the middle
// and the end, so a reader frozen at an older root or answering
// nothing fails the first half, and one that picks up later writes fails the
// second. A store opened after the writes must see the later edges, which keeps
// the second half from passing on writes that never landed.
func TestGetRelationsByIDsAnswersFromTheMomentItsStoreOpened(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	writer, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	fx := newBatchedRelationFixture(t, ctx, writer)
	last := len(fx.subjects) - 1
	early := []int{0, last / 2, last}
	late := []int{1, last/2 + 1, last - 1}
	fx.wire(t, ctx, writer, early)

	reader, err := OpenForRead(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() beside a live writer error = %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	fx.wire(t, ctx, writer, late)

	before, err := reader.GetRelationsByIDs(ctx, fx.subjects)
	if err != nil {
		t.Fatalf("reader GetRelationsByIDs(%d ids) error = %v", len(fx.subjects), err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer Close() error = %v", err)
	}
	later, err := OpenForRead(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() after the writes error = %v", err)
	}
	t.Cleanup(func() { _ = later.Close() })
	after, err := later.GetRelationsByIDs(ctx, fx.subjects)
	if err != nil {
		t.Fatalf("later GetRelationsByIDs(%d ids) error = %v", len(fx.subjects), err)
	}

	for _, i := range early {
		id := fx.subjects[i]
		if got := before[id]; len(got.BlockedBy) != 1 || len(got.Blocks) != 1 {
			t.Fatalf("subject %d of %d (%s) wired before the reader opened: BlockedBy = %v, Blocks = %v, want one edge each — the reader did not answer from the moment it opened", i, len(fx.subjects), id, ids(got.BlockedBy), ids(got.Blocks))
		}
	}
	for _, i := range late {
		id := fx.subjects[i]
		if got := before[id]; len(got.BlockedBy) != 0 || len(got.Blocks) != 0 {
			t.Fatalf("subject %d of %d (%s) wired after the reader opened: BlockedBy = %v, Blocks = %v, want both empty — the reader saw a write made after it opened", i, len(fx.subjects), id, ids(got.BlockedBy), ids(got.Blocks))
		}
		if got := after[id]; len(got.BlockedBy) != 1 || len(got.Blocks) != 1 {
			t.Fatalf("subject %d of %d (%s) read after the writes: BlockedBy = %v, Blocks = %v, want one edge each — the writes never landed, so the check above proved nothing", i, len(fx.subjects), id, ids(got.BlockedBy), ids(got.Blocks))
		}
	}
}

// batchedRelationFixture is an epic holding a few dozen subjects, plus the two
// peers their edges run to.
//
// The two endpoint queries need edges that only one of them can see. Every
// edge wire makes runs between a queried subject and a peer that is never
// queried, so a subject's edge to sink is reachable only through the src_id
// query and its edge from source only through the dst_id query.
//
// Wiring both directions to one shared peer instead would cover neither: if
// that peer were itself queried, whichever query reached it would return the
// whole edge set and answer for the other, and the first truncation aimed at
// either would pass.
type batchedRelationFixture struct {
	subjects     []string
	sink, source model.Issue
}

func newBatchedRelationFixture(t *testing.T, ctx context.Context, st *Store) batchedRelationFixture {
	t.Helper()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Batch epic", Topic: "batch", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	const subjectCount = 34
	subjects := make([]string, 0, subjectCount)
	for i := range subjectCount {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Prefix: "test", Title: fmt.Sprintf("Subject %d", i), Topic: "batch",
			IssueType: "task", Placement: storage.RankBottom, ParentID: epic.ID,
		})
		if err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		subjects = append(subjects, issue.ID)
	}
	// Sink and source sit in an epic of their own: between two leaves of one
	// epic a blocked-by edge is refused.
	ends, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Ends epic", Topic: "batch", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(ends epic) error = %v", err)
	}
	sink, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Sink", Topic: "batch", IssueType: "task", ParentID: ends.ID})
	if err != nil {
		t.Fatalf("CreateIssue(sink) error = %v", err)
	}
	source, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Source", Topic: "batch", IssueType: "task", ParentID: ends.ID})
	if err != nil {
		t.Fatalf("CreateIssue(source) error = %v", err)
	}
	return batchedRelationFixture{subjects: subjects, sink: sink, source: source}
}

// wire gives each named subject one edge through each endpoint column: it is
// blocked by sink, and it blocks source.
func (fx batchedRelationFixture) wire(t *testing.T, ctx context.Context, st *Store, subjects []int) {
	t.Helper()
	for _, i := range subjects {
		mustRelate(t, ctx, st, fx.subjects[i], fx.sink.ID, "blocked-by")
		mustRelate(t, ctx, st, fx.source.ID, fx.subjects[i], "blocked-by")
	}
}
