package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The backstop runs after mutating commands, so what keeps it off every
// command's latency path is the probe interval — without it, every mutation
// would pay an engine open to ask a question whose answer is almost always
// "nothing is due". A fresh marker must block; an aged one must allow.
func TestCompactProbeIntervalGatesTheBackstop(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
	now := time.Now()

	if !shouldRunNow(compactMarkerPath(ws), now, compactProbeInterval) {
		t.Fatal("a workspace that has never been probed must be allowed to probe")
	}

	if err := markRunAttempt(ws, compactMarkerPath(ws)); err != nil {
		t.Fatalf("markRunAttempt error = %v", err)
	}
	if _, err := os.Stat(compactMarkerPath(ws)); err != nil {
		t.Fatalf("marker not created: %v", err)
	}

	if shouldRunNow(compactMarkerPath(ws), now.Add(time.Second), compactProbeInterval) {
		t.Fatal("a probe inside the interval must be blocked; otherwise every mutation opens an engine")
	}
	if !shouldRunNow(compactMarkerPath(ws), now.Add(compactProbeInterval+time.Second), compactProbeInterval) {
		t.Fatal("a probe past the interval must be allowed; otherwise the store stops being maintained")
	}
}

// The marker is written BEFORE the pass, not after, which is what bounds a
// store that fails every attempt to one try per interval rather than one per
// command. A marker written only on success would leave a broken store stalling
// every mutation forever.
func TestCompactMarkerIsDistinctFromTheReceiveMarker(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}

	if compactMarkerPath(ws) == receiveMarkerPath(ws) {
		t.Fatal("compaction and receive share a debounce marker; one would silently suppress the other")
	}
}

// The mirror waits a bounded time for its spawning parent to release the
// engine, and that bound must sit above everything the parent legitimately
// schedules after the spawn — the compaction backstop included. A bound inside
// the parent's own tail abandons a mirror that owed a push, for work the parent
// was designed to do.
//
// This asserts the relationship rather than either number, so retuning any term
// stays free and only breaking the ordering fails. [LAW:behavior-not-structure]
func TestMirrorParentWaitExceedsThePostSpawnTail(t *testing.T) {
	t.Parallel()

	if parentPostSpawnTail < compactTimeout {
		t.Fatalf("parentPostSpawnTail (%s) omits compactTimeout (%s); a step that runs after the spawn must be summed into the tail",
			parentPostSpawnTail, compactTimeout)
	}
	if mirrorParentWaitTimeout <= parentPostSpawnTail {
		t.Fatalf("mirrorParentWaitTimeout (%s) is inside the parent's designed tail (%s); a healthy parent would be abandoned mid-tail",
			mirrorParentWaitTimeout, parentPostSpawnTail)
	}
}

// compactSyncer answers SyncCompact and nothing else. The interface is embedded
// rather than hand-stubbed so that a handler reaching for any other capability
// panics on the nil call instead of quietly receiving a zero value — the fake
// fails loudly at exactly the point a stub would have lied.
// [LAW:no-silent-failure]
type compactSyncer struct {
	storage.Syncer
	gotMode storage.GCMode
	outcome storage.CompactionOutcome
	err     error
}

func (s *compactSyncer) SyncCompact(_ context.Context, mode storage.GCMode) (storage.CompactionOutcome, error) {
	s.gotMode = mode
	return s.outcome, s.err
}

// refusingWriter is a stdout that has gone away — a closed pipe, a full disk.
type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout is gone") }

// readSyncTraces returns every sync trace this workspace recorded, so a test can
// assert the durable trail an operator or `lit doctor` would read later rather
// than only the output that scrolled past.
func readSyncTraces(t *testing.T, ws workspace.Info) []syncTraceRecord {
	t.Helper()
	dir := syncTraceDir(ws)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read sync trace dir: %v", err)
	}
	records := make([]syncTraceRecord, 0, len(entries))
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read trace %s: %v", entry.Name(), err)
		}
		var record syncTraceRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			t.Fatalf("parse trace %s: %v", entry.Name(), err)
		}
		records = append(records, record)
	}
	return records
}

func compactWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	return workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
}

// The depth is the whole point of this command, so the flag that selects it, the
// account it prints, and the record it leaves are its contract. Asserting all
// three together is what makes the handler's behavior visible — the depth alone
// would pass while the operator was told nothing, and the output alone would
// pass while the durable trail stayed empty. [LAW:behavior-not-structure]
func TestRunSyncCompactCarriesTheDepthAndReportsThePass(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want storage.GCMode
	}{
		{"the bare form takes the shallow depth", nil, storage.GCNewGen},
		{"--full reaches the deep one", []string{"--full"}, storage.GCFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := compactWorkspace(t)
			syncer := &compactSyncer{outcome: storage.CompactionOutcome{
				Ran: true, Depth: tc.want, Detail: "journal 4.0 KiB -> 0 B",
			}}
			var out bytes.Buffer

			if err := runSyncCompact(context.Background(), &out, ws, syncSession{syncer: syncer}, tc.args); err != nil {
				t.Fatalf("runSyncCompact() error = %v", err)
			}

			if syncer.gotMode != tc.want {
				t.Fatalf("engine was asked for depth %v, want %v", syncer.gotMode, tc.want)
			}
			want := "compacted (" + tc.want.String() + "): journal 4.0 KiB -> 0 B\n"
			if out.String() != want {
				t.Fatalf("stdout = %q, want %q", out.String(), want)
			}
			traces := readSyncTraces(t, ws)
			if len(traces) != 1 {
				t.Fatalf("recorded %d traces, want exactly one", len(traces))
			}
			if traces[0].Decision != "compacted" || traces[0].Status != "ok" {
				t.Fatalf("trace = %+v, want a recorded success", traces[0])
			}
			if traces[0].Metadata["depth"] != tc.want.String() {
				t.Fatalf("trace depth = %q, want %q — the trail cannot tell a shallow pass from a deep one without it",
					traces[0].Metadata["depth"], tc.want.String())
			}
			if traces[0].Metadata["detail"] != "journal 4.0 KiB -> 0 B" {
				t.Fatalf("trace detail = %q, want the engine's own account — a scheduled pass may have nobody reading its stdout, leaving this the only record of what it reclaimed",
					traces[0].Metadata["detail"])
			}
		})
	}
}

// Both compaction paths record through one renderer, so the durable trail
// carries a single shape whichever entry point ran. They previously spelled
// their own keys and drifted — the backstop writing "mode", the explicit
// command "depth", and only the backstop carrying the detail — so a reader had
// to know which path ran before it knew which key to read. Pinning the
// vocabulary here is what keeps a third call site from inventing a fourth
// spelling. [LAW:one-source-of-truth]
func TestCompactionTraceMetadataRendersOneShapeForEveryPath(t *testing.T) {
	t.Parallel()

	got := compactionTraceMetadata(storage.CompactionOutcome{
		Ran: true, Depth: storage.GCFull, Detail: "journal 1.0 KiB -> 0 B",
	})
	want := map[string]string{"depth": "full", "detail": "journal 1.0 KiB -> 0 B"}

	if len(got) != len(want) {
		t.Fatalf("metadata = %v, want exactly the keys %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("metadata[%q] = %q, want %q", key, got[key], value)
		}
	}
}

// A pass that failed has to reach both the caller and the durable trail: the
// exit status is the operator's signal now, the trace is the record later, and a
// failure present in only one of them leaves the other lying.
// [LAW:no-silent-failure]
func TestRunSyncCompactSurfacesAndTracesAFailedPass(t *testing.T) {
	t.Parallel()
	ws := compactWorkspace(t)
	failure := errors.New("dolt gc: store is read-only")
	var out bytes.Buffer

	err := runSyncCompact(context.Background(), &out, ws, syncSession{syncer: &compactSyncer{err: failure}}, nil)

	if !errors.Is(err, failure) {
		t.Fatalf("runSyncCompact() error = %v, want the engine's own failure", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing printed for a pass that never ran", out.String())
	}
	traces := readSyncTraces(t, ws)
	if len(traces) != 1 || traces[0].Status != "error" {
		t.Fatalf("traces = %+v, want exactly one recorded error", traces)
	}
	if traces[0].Reason != failure.Error() {
		t.Fatalf("trace reason = %q, want the engine's message %q", traces[0].Reason, failure.Error())
	}
}

// A store that was compacted but could not say so is not a successful run, so
// the write's failure is the command's. The trace still has to survive it: the
// pass really happened, and the record of it must not depend on stdout still
// being there to hear about it — which is why the trace is written first.
// [LAW:no-silent-failure]
func TestRunSyncCompactFailsWhenItCannotReportThePass(t *testing.T) {
	t.Parallel()
	ws := compactWorkspace(t)
	syncer := &compactSyncer{outcome: storage.CompactionOutcome{
		Ran: true, Depth: storage.GCNewGen, Detail: "journal 4.0 KiB -> 0 B",
	}}

	err := runSyncCompact(context.Background(), refusingWriter{}, ws, syncSession{syncer: syncer}, nil)

	if err == nil {
		t.Fatal("runSyncCompact() = nil against a stdout that refused the write; a compacted store that could not report is not a success")
	}
	traces := readSyncTraces(t, ws)
	if len(traces) != 1 || traces[0].Decision != "compacted" {
		t.Fatalf("traces = %+v, want the pass recorded despite the failed write", traces)
	}
}
