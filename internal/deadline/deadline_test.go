package deadline

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The sizing chains are arithmetic over measured facts, so the first tests
// assert the consts Shipped is made of: the design is what is being pinned,
// not a figure a test hands a run.

// TestMirrorHoldBudgetExceedsObservedCloneCost checks the budget against the
// cost of the work it bounds. A check that the budget fits inside the
// foreground's retry would pass a budget of one second.
//
// A hold budget is a stall detector. Its whole claim is that work still running
// at the deadline has stopped making progress, and that claim is false the
// moment the deadline lands inside the distribution of healthy runs. So the
// check is against mirrorCloneObservedTail — the slowest healthy clone step
// ever measured — and it demands real separation, not a hair above it.
// [LAW:verifiable-goals] the relation is a number, so it is checked, not prose.
func TestMirrorHoldBudgetExceedsObservedCloneCost(t *testing.T) {
	t.Parallel()
	if mirrorHoldStallFactor < 2 {
		t.Fatalf("mirrorHoldStallFactor is %d; a budget under twice the slowest healthy hold fires on healthy holds",
			mirrorHoldStallFactor)
	}
	if mirrorHoldBudget <= mirrorCloneObservedTail {
		t.Fatalf("mirrorHoldBudget (%s) does not exceed mirrorCloneObservedTail (%s); the budget is sized inside the cost of the work it bounds, so healthy holds are cut and reported as faults",
			mirrorHoldBudget, mirrorCloneObservedTail)
	}
}

// TestMirrorHoldBudgetIsAHoldNotARoundTrip pins that the mirror's hold on the
// live store is a local clone, so its budget must sit far under the network
// round trip. A hold budget that could accommodate a push would mean the push
// had crept under the hold — a wait every co-resident command would pay.
func TestMirrorHoldBudgetIsAHoldNotARoundTrip(t *testing.T) {
	t.Parallel()
	if mirrorHoldBudget >= mirrorPushObservedTail {
		t.Fatalf("mirrorHoldBudget (%s) is at or above the slowest healthy push (%s); the hold on the live store is a clone, not a round trip, and its budget must say so",
			mirrorHoldBudget, mirrorPushObservedTail)
	}
	if ceiling := Shipped().HoldCeiling(); ceiling >= time.Second*2 {
		t.Fatalf("the shipped hold ceiling (%s) is not sub-two-second; the contract is a hold under one second on every healthy cycle, and a ceiling this high means the budget is not sized from the clone's measured cost",
			ceiling)
	}
}

// TestMirrorPushDeadlineExceedsObservedPushCost pins the push deadline against
// the operation it bounds, exactly as the hold budget is pinned against the
// clone: the push holds nothing on the live store, but a deadline inside the
// distribution of healthy pushes still cuts healthy pushes and reports each as
// a transport fault.
func TestMirrorPushDeadlineExceedsObservedPushCost(t *testing.T) {
	t.Parallel()
	if mirrorPushStallFactor < 2 {
		t.Fatalf("mirrorPushStallFactor is %d; a deadline under twice the slowest healthy push fires on healthy pushes",
			mirrorPushStallFactor)
	}
	if mirrorPushDeadline <= mirrorPushObservedTail {
		t.Fatalf("mirrorPushDeadline (%s) does not exceed mirrorPushObservedTail (%s); the deadline is sized inside the cost of the work it bounds",
			mirrorPushDeadline, mirrorPushObservedTail)
	}
}

// TestTheShippedHolderWaitIsSizedToTheMirrorHold pins the relation between the
// wait and the hold. A foreground write open waits out a co-resident holder
// for HolderWait, and the holder it is sized for is the mirror — so the wait
// has to outlast the mirror's longest LEGAL hold. That hold is not the budget:
// a cut does not land on its deadline, so a wait sized against the budget
// alone is sized against a number the hold does not respect.
//
// The other half of the same relation is the contract that a command that
// cannot get the store fails within five seconds. The wait, plus one poll and
// one of dolt's own 100ms attempts (a refusal can land that long after the
// wait elapses), stays inside it.
func TestTheShippedHolderWaitIsSizedToTheMirrorHold(t *testing.T) {
	t.Parallel()
	d := Shipped()
	if d.HolderWait() <= d.HoldCeiling() {
		t.Fatalf("HolderWait (%s) does not outlast HoldCeiling (%s); a foreground open can starve against a legal mirror hold and fail as though the workspace were wedged",
			d.HolderWait(), d.HoldCeiling())
	}
	if d.HoldCeiling() <= d.MirrorHold() {
		t.Fatalf("HoldCeiling (%s) does not exceed MirrorHold (%s); the ceiling exists because a cut does not land on its deadline, and a ceiling equal to the budget restates the budget instead of correcting it",
			d.HoldCeiling(), d.MirrorHold())
	}
	if refusal := d.HolderWait() + 2*LockPollInterval; refusal >= 5*time.Second {
		t.Fatalf("a contender's refusal lands after %s; the contract is that a command that cannot get the store fails within five seconds", refusal)
	}
}

// A run that lifts its hold lifts every co-resident waiter's patience with it:
// the wait is the run's, not the shipped figure's.
func TestTheHolderWaitFollowsTheRunsHold(t *testing.T) {
	t.Parallel()
	f := Shipped().Figures()
	f.MirrorHold = time.Minute
	d, err := Of(f)
	if err != nil {
		t.Fatal(err)
	}
	if d.HolderWait() <= time.Minute {
		t.Fatalf("HolderWait under a one-minute hold = %s; a contender would give up on a legal hold", d.HolderWait())
	}
}

// Of builds deadlines only from positive figures: each figure left out or
// below zero is refused by name, where the same figures with it set build.
func TestOfRefusesAFigureThatIsNotPositive(t *testing.T) {
	t.Parallel()
	whole := Shipped().Figures()
	if _, err := Of(whole); err != nil {
		t.Fatalf("Of(the shipped figures) = %v", err)
	}
	unsets := map[string]func(*Figures){
		"mirror hold deadline":  func(f *Figures) { f.MirrorHold = 0 },
		"mirror push deadline":  func(f *Figures) { f.MirrorPush = -time.Second },
		"receive deadline":      func(f *Figures) { f.Receive = 0 },
		"push proof deadline":   func(f *Figures) { f.PushProof = 0 },
		"compaction deadline":   func(f *Figures) { f.Compaction = 0 },
		"re-root hold deadline": func(f *Figures) { f.ReRootHold = 0 },
		"engine close reserve":  func(f *Figures) { f.EngineCloseReserve = 0 },
	}
	if fields := reflect.TypeFor[Figures]().NumField(); len(unsets) != fields {
		t.Fatalf("this test unsets %d figures of the %d Figures has; each must be refused by its name", len(unsets), fields)
	}
	for name, unset := range unsets {
		f := whole
		unset(&f)
		if _, err := Of(f); err == nil || !strings.Contains(err.Error(), "the "+name+" must be positive") {
			t.Errorf("Of with no %s = %v, want it refused by name", name, err)
		}
	}
}

// Deadlines cross a process boundary as JSON and come back as the same
// deadlines; JSON carrying a figure that is missing is refused as it is read.
func TestDeadlinesReadBackAsWrittenAndRefuseAMissingFigure(t *testing.T) {
	t.Parallel()
	f := Shipped().Figures()
	f.Receive = 3 * time.Second
	sent, err := Of(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var got Deadlines
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("read back %s: %v", data, err)
	}
	if got != sent {
		t.Fatalf("read back %+v, want %+v", got.Figures(), sent.Figures())
	}
	var short Deadlines
	err = json.Unmarshal([]byte(`{"mirror_hold":1,"mirror_push":1,"receive":1,"push_proof":1}`), &short)
	if err == nil || !strings.Contains(err.Error(), "the compaction deadline must be positive") {
		t.Fatalf("read deadlines missing the compaction figure = %v, want it refused", err)
	}
}

// The zero Deadlines is no run's, so reading a figure from it fails loudly
// rather than reading as a deadline of zero.
func TestReadingTheZeroDeadlinesPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("reading the zero Deadlines' hold returned; want a panic naming Of")
		}
	}()
	_ = Deadlines{}.MirrorHold()
}
