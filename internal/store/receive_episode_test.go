package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestASettleRecordsThatItSettled pins the settle as the writer of
// settled.last: a settle that succeeds writes it, whatever state it settled
// to; a settle that fails writes nothing; and a settle whose record cannot be
// written still returns its result with no error, because the record is
// bookkeeping after the settle completed, while the write's failure lands on
// the engine's SettledLastWrites. [LAW:no-silent-failure] [LAW:nothing-unseen]
func TestASettleRecordsThatItSettled(t *testing.T) {
	t.Parallel()

	t.Run("a settle that fails writes nothing", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		st := f.openSync(f.live)
		defer f.close(st)
		canceled, cancel := context.WithCancel(f.ctx)
		cancel()
		if _, err := st.SyncSettleReceived(canceled, remoteName(t, "origin"), "master"); err == nil {
			t.Fatal("SyncSettleReceived() on a canceled context succeeded; the case needs a settle that fails")
		}
		if _, ok, err := ReadSettledLast(f.live); ok || err != nil {
			t.Fatalf("ReadSettledLast() after a failed settle = ok %v, err %v; want no record", ok, err)
		}
	})

	t.Run("a settle that succeeds writes when", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		st := f.openSync(f.live)
		defer f.close(st)
		before := time.Now()
		result, err := st.SyncSettleReceived(f.ctx, remoteName(t, "origin"), "master")
		if err != nil {
			t.Fatalf("SyncSettleReceived() error = %v", err)
		}
		at, ok, err := ReadSettledLast(f.live)
		if err != nil || !ok || at.Before(before.Add(-time.Second)) || at.After(time.Now()) {
			t.Fatalf("ReadSettledLast() after a %s settle = %s ok %v err %v; want the settle's time", result.State, at, ok, err)
		}
		if writes := st.SettledLastWrites(); len(writes) != 1 || writes[0].Err != "" {
			t.Fatalf("SettledLastWrites() = %+v; want one write that succeeded", writes)
		}
	})

	t.Run("a record that cannot be written leaves the settle standing", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		peer := f.copyOf(f.live)
		f.commit(peer, "peer-ticket")
		f.push(peer, false)
		clone := f.fetchedClone()
		if _, err := LandFetchedHead(f.ctx, f.live, clone, remoteName(t, "origin"), "master", nil); err != nil {
			t.Fatalf("LandFetchedHead() error = %v", err)
		}
		if err := os.RemoveAll(filepath.Dir(clone.root)); err != nil {
			t.Fatalf("remove clone: %v", err)
		}
		if err := os.MkdirAll(SettledLastPath(f.live), 0o755); err != nil {
			t.Fatalf("block the record: %v", err)
		}
		st := f.openSync(f.live)
		defer f.close(st)
		result, err := st.SyncSettleReceived(f.ctx, remoteName(t, "origin"), "master")
		if err != nil || result.State != storage.SyncReceiveFastForwarded || result.Behind != 1 {
			t.Fatalf("SyncSettleReceived() = %+v, %v; want the fast-forward, no error", result, err)
		}
		if writes := st.SettledLastWrites(); len(writes) != 1 || !strings.Contains(writes[0].Err, "settled.last") {
			t.Fatalf("SettledLastWrites() = %+v; want the one write's failure", writes)
		}
	})
}

// TestTheReceiveRecordsRoundTrip pins the records' encoding: what is written
// is read back to the nanosecond, absence is a value, and a record that does
// not parse is an error rather than a zero time. [LAW:parse-dont-validate]
func TestTheReceiveRecordsRoundTrip(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "dolt")
	if _, ok, err := ReadUnsettledFirst(databasePath); ok || err != nil {
		t.Fatalf("ReadUnsettledFirst() with no record = ok %v err %v; want absent", ok, err)
	}
	at := time.Date(2026, 10, 20, 11, 22, 33, 444555666, time.UTC)
	if err := WriteUnsettledFirst(databasePath, at); err != nil {
		t.Fatalf("WriteUnsettledFirst() error = %v", err)
	}
	if got, ok, err := ReadUnsettledFirst(databasePath); !ok || err != nil || !got.Equal(at) {
		t.Fatalf("ReadUnsettledFirst() = %s ok %v err %v; want %s", got, ok, err, at)
	}
	if err := os.WriteFile(SettledLastPath(databasePath), []byte("not a time\n"), 0o600); err != nil {
		t.Fatalf("write a torn record: %v", err)
	}
	if _, ok, err := ReadSettledLast(databasePath); ok || err == nil {
		t.Fatalf("ReadSettledLast() of a record that does not parse = ok %v err %v; want an error", ok, err)
	}
}
