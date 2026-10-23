package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// divergeAndReconcile drives two checkouts of one remote through rounds of
// ordinary use: A writes and pushes, B writes locally, B fetches, reconciles
// the divergence and pushes, and A receives. Every round's reconcile takes a
// pre-reconcile recovery snapshot on B, the way concurrent agents on one
// remote produce them.
func divergeAndReconcile(t *testing.T, ctx context.Context, rounds int) (rootA, rootB string) {
	t.Helper()
	rootA = migratedDoltDir(t)
	rootB = unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)
	origin := remoteName(t, "origin")
	for round := range rounds {
		updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Lane: strptr(fmt.Sprintf("lane-%d", round))})
		priority := model.PriorityUrgent
		if round%2 == 1 {
			priority = model.PriorityNormal
		}
		updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Priority: ptr(priority)})

		syncB := openSyncOrFatal(t, ctx, rootB)
		if err := syncB.SyncFetch(ctx, origin); err != nil {
			t.Fatalf("round %d: SyncFetch(B): %v", round, err)
		}
		res, err := syncB.SyncReconcile(ctx, origin, "master")
		if err != nil {
			t.Fatalf("round %d: SyncReconcile(B): %v", round, err)
		}
		if res.State != storage.SyncReconcileLinearized {
			t.Fatalf("round %d: reconcile state = %q, want %q", round, res.State, storage.SyncReconcileLinearized)
		}
		if _, err := syncB.SyncPush(ctx, origin, "master", false, false); err != nil {
			t.Fatalf("round %d: SyncPush(B): %v", round, err)
		}
		if err := syncB.Close(); err != nil {
			t.Fatalf("round %d: Close(B): %v", round, err)
		}

		syncA := openSyncOrFatal(t, ctx, rootA)
		if _, err := syncA.SyncReceive(ctx, origin, "master"); err != nil {
			t.Fatalf("round %d: SyncReceive(A): %v", round, err)
		}
		if err := syncA.Close(); err != nil {
			t.Fatalf("round %d: Close(A): %v", round, err)
		}
	}
	return rootA, rootB
}

// measureOrFatal reads a store's footprint the way MaintainIfDue does.
func measureOrFatal(t *testing.T, doltRoot string) footprint {
	t.Helper()
	f, err := measureFootprint(doltRoot)
	if err != nil {
		t.Fatalf("measureFootprint(%s): %v", doltRoot, err)
	}
	return f
}

// newestPerKind names the newest automatic snapshot of each kind f holds:
// what retention never retires.
func newestPerKind(f footprint) map[string]string {
	newest := map[string]string{}
	for _, snap := range f.automatic {
		if _, ok := newest[snap.kind]; !ok {
			newest[snap.kind] = snap.Name
		}
	}
	return newest
}

// A store pushed past its ceiling by ordinary use — writes, pushes and
// fetches on two checkouts of one remote, whose divergences each leave a
// recovery snapshot — comes back under it through the backstop's own
// entrypoint, with no compact or prune asked for. This is the ticket's
// done-when: the ceiling holds itself.
func TestOrdinaryUsePastTheCeilingComesBackUnderOnItsOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const rounds = 4
	_, rootB := divergeAndReconcile(t, ctx, rounds)

	before := measureOrFatal(t, rootB)
	var reconciles []sizedSnapshot
	for _, snap := range before.automatic {
		if snap.kind == reconcileSnapshotLabel {
			reconciles = append(reconciles, snap)
		}
	}
	if len(reconciles) != rounds {
		t.Fatalf("B holds %d pre-reconcile snapshots after %d reconciles, want one each — the test cannot show retention without them", len(reconciles), rounds)
	}

	// The ceiling is what B's own measurement says: room for everything
	// retention cannot touch, the newest snapshot of each kind, and one more
	// reconcile snapshot. Ordinary use left B holding more than that.
	newest := newestPerKind(before)
	ceiling := before.chunkStore + before.tempTableFiles + before.otherSnapshots + reconciles[1].bytes
	for _, snap := range before.automatic {
		if newest[snap.kind] == snap.Name {
			ceiling += snap.bytes
		}
	}
	if _, over := before.contract(ceiling).Over(); !over {
		t.Fatalf("B's footprint %+v is within the derived ceiling %d; ordinary use did not drive it past", before.contract(ceiling), ceiling)
	}

	st := openSyncOrFatal(t, ctx, rootB)
	t.Cleanup(func() { _ = st.Close() })
	st.sizeCeiling = ceiling
	outcome, err := st.MaintainIfDue(ctx)
	if err != nil {
		t.Fatalf("MaintainIfDue(): %v", err)
	}

	if over, isOver := outcome.Footprint.Over(); isOver {
		t.Fatalf("after MaintainIfDue the store is still over its ceiling by %d (%+v)", over.Excess, outcome.Footprint)
	}
	after := measureOrFatal(t, rootB)
	if governed := after.contract(ceiling).GovernedBytes(); governed > ceiling {
		t.Fatalf("on disk after MaintainIfDue: %d governed bytes against a %d ceiling — the outcome and the disk disagree", governed, ceiling)
	}
	if len(outcome.SnapshotsPruned) == 0 {
		t.Fatal("MaintainIfDue pruned nothing, yet the store came back under; the test is not exercising retention")
	}
	held := newestPerKind(after)
	for kind, name := range newest {
		if held[kind] != name {
			t.Fatalf("the newest %s snapshot %s did not survive retention (newest now %q)", kind, name, held[kind])
		}
	}
	if surviving := reconciles[1].Name; !strings.Contains(strings.Join(snapshotNames(after), ","), surviving) {
		t.Fatalf("the second-newest reconcile snapshot %s was retired, though the ceiling left room for it", surviving)
	}
}

func snapshotNames(f footprint) []string {
	out := make([]string, 0, len(f.automatic))
	for _, snap := range f.automatic {
		out = append(out, snap.Name)
	}
	return out
}

// A store whose chunk store alone exceeds its ceiling has nothing lit may
// reclaim. Retention still keeps the newest recovery point of each kind, and
// the store says which part holds the bytes: the probe's outcome and doctor
// both name the chunk store. Over the ceiling costs disk, never correctness,
// so doctor reports it among warnings and raises no error.
func TestAStoreThatCannotShrinkNamesThePartHoldingTheBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, rootB := divergeAndReconcile(t, ctx, 2)
	before := measureOrFatal(t, rootB)
	newest := newestPerKind(before)

	st := openSyncOrFatal(t, ctx, rootB)
	t.Cleanup(func() { _ = st.Close() })
	st.sizeCeiling = before.chunkStore / 2
	outcome, err := st.MaintainIfDue(ctx)
	if err != nil {
		t.Fatalf("MaintainIfDue(): %v", err)
	}

	over, isOver := outcome.Footprint.Over()
	if !isOver || over.Largest.Name != "chunk store" {
		t.Fatalf("outcome over = %v naming %q, want over naming the chunk store (%+v)", isOver, over.Largest.Name, outcome.Footprint)
	}
	if held := newestPerKind(measureOrFatal(t, rootB)); len(held) != len(newest) {
		t.Fatalf("after MaintainIfDue the newest snapshot per kind = %v, want %v kept", held, newest)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(): %v", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("doctor errors = %v, want none — a store over its ceiling is a warning", report.Errors)
	}
	var named bool
	for _, warning := range report.Warnings {
		named = named || (strings.Contains(warning, "over its size ceiling") && strings.Contains(warning, "chunk store"))
	}
	if !named {
		t.Fatalf("doctor warnings = %v, want one naming the chunk store as holding the bytes", report.Warnings)
	}
	if !report.Footprint.Measured() || report.Footprint.Ceiling != st.sizeCeiling {
		t.Fatalf("doctor footprint = %+v, want one measured under the handle's ceiling %d", report.Footprint, st.sizeCeiling)
	}
}

// A footprint that cannot be read is a warning in doctor's report, never the
// end of it: the integrity checks still run, and the report's footprint reads
// as unmeasured rather than as an empty store. The size is read from a
// directory doctor cannot list, which is the failure a walk meets in the field.
func TestDoctorReportsAnUnreadableFootprintAndStillChecks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	t.Parallel()
	ctx := context.Background()
	st := openFreshWorkspace(t)
	sealed := filepath.Join(remoteCacheBaseFor(st.doltRootDir), "sealed")
	if err := os.MkdirAll(sealed, 0o755); err != nil {
		t.Fatalf("make the sealed cache dir: %v", err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatalf("seal the cache dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(): %v, want the unreadable footprint reported, not returned", err)
	}
	if report.IntegrityCheck != "ok" {
		t.Fatalf("integrity check = %q, want the checks run past the footprint", report.IntegrityCheck)
	}
	if report.Footprint.Measured() {
		t.Fatalf("doctor footprint = %+v, want unmeasured", report.Footprint)
	}
	var named bool
	for _, warning := range report.Warnings {
		named = named || (strings.Contains(warning, "store footprint not measured") && strings.Contains(warning, "permission denied"))
	}
	if !named {
		t.Fatalf("doctor warnings = %v, want one saying the footprint was not measured and why", report.Warnings)
	}
}
