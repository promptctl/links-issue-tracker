package claims_test

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

const (
	epicID      = "E"
	workspaceID = "ws-local"
	window      = 24 * time.Hour
)

var (
	now = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	// Two checkouts of the same workspace, plus one from a different clone —
	// the pair that makes the local liveness prune's workspace scoping testable.
	streamA = model.NewAttribution("aaaaaaaa", workspaceID)
	streamB = model.NewAttribution("bbbbbbbb", workspaceID)
	foreign = model.NewAttribution("cccccccc", "ws-elsewhere")

	fresh = claims.Freshness{Now: now, Window: window}

	// bothLive is a machine that enumerated its worktrees and found both local
	// streams present. assumeLive is the zero value — a machine that cannot
	// check and therefore prunes nothing.
	bothLive   = claims.NewLocalCheckouts(workspaceID, []string{streamA.Stream(), streamB.Stream()})
	assumeLive = claims.LocalCheckouts{}
)

func ago(d time.Duration) time.Time { return now.Add(-d) }

// leaf builds a hydrated ticket. Claim derivation reads lifecycle state, so an
// unhydrated issue would panic rather than answer.
func leaf(t *testing.T, id, lane string, state model.State) model.Issue {
	t.Helper()
	issue, err := model.HydrateStatus(
		model.Issue{ID: id, Lane: lane, IssueType: model.TypeTask},
		model.StatusView{Value: state},
	)
	if err != nil {
		t.Fatalf("hydrate %s: %v", id, err)
	}
	return issue
}

func container(t *testing.T, id string, children ...model.Issue) model.Issue {
	t.Helper()
	issue, err := model.HydrateAllOf(model.Issue{ID: id, IssueType: model.TypeEpic}, children)
	if err != nil {
		t.Fatalf("hydrate epic %s: %v", id, err)
	}
	return issue
}

// laneIn and soloLane name a lane the way a caller would, through the only
// constructor there is. Neither needs a hydrated issue, because lane identity is
// a structural fact — the epic's id and the lane's spelling — and reads no
// lifecycle at all.
func laneIn(epic, key string) model.LaneID {
	parent := model.Issue{ID: epic, IssueType: model.TypeEpic}
	return model.LaneOf(model.Issue{ID: "any-member", Lane: key}, &parent)
}

func soloLane(id string) model.LaneID {
	return model.LaneOf(model.Issue{ID: id}, nil)
}

func event(id, issueID string, action model.ActionName, at time.Time, by model.Attribution) model.IssueEvent {
	return model.IssueEvent{ID: id, IssueID: issueID, Action: string(action), CreatedAt: at, Attribution: by}
}

// epicOf assembles an epic over the given children and returns the parent map
// NewEvidence wants, so every case states only the children it cares about.
func epicOf(t *testing.T, children ...model.Issue) (issues []model.Issue, parents map[string]*model.Issue) {
	t.Helper()
	epic := container(t, epicID, children...)
	issues = append([]model.Issue{epic}, children...)
	parents = map[string]*model.Issue{}
	for _, child := range children {
		parents[child.ID] = &epic
	}
	return issues, parents
}

func derive(t *testing.T, issues []model.Issue, parents map[string]*model.Issue, events []model.IssueEvent, local claims.LocalCheckouts) claims.Standings {
	t.Helper()
	evidence, err := claims.NewEvidence(issues, parents, events)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}
	return claims.Derive(evidence, fresh, local)
}

// held is shorthand for the expected standing of an uncontested holder.
func held(by model.Attribution, since, last time.Time) claims.Held {
	return claims.Held{Tenure: claims.Tenure{By: by, Since: since, LastActivity: last}}
}

// TestPredicateGrid drops each leg of the claim predicate in turn and watches
// the claim dissolve. Every case shares one shape — an epic with two open
// tickets in its default lane, worked by stream A — so the only thing a row
// changes is the leg it is dropping.
func TestPredicateGrid(t *testing.T) {
	twoOpen := func(t *testing.T) []model.Issue {
		return []model.Issue{leaf(t, "T1", "", model.StateOpen), leaf(t, "T2", "", model.StateOpen)}
	}

	cases := []struct {
		name     string
		children func(*testing.T) []model.Issue
		events   []model.IssueEvent
		local    claims.LocalCheckouts
		want     claims.Standing
	}{
		{
			name:     "all four legs hold: the lane is held",
			children: twoOpen,
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionStart, ago(2*time.Hour), streamA),
			},
			local: bothLive,
			want:  held(streamA, ago(2*time.Hour), ago(2*time.Hour)),
		},
		{
			name: "leg 1 dropped — the lane is finished, so there is nothing to hold",
			children: func(t *testing.T) []model.Issue {
				return []model.Issue{leaf(t, "T1", "", model.StateClosed), leaf(t, "T2", "", model.StateClosed)}
			},
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionStart, ago(2*time.Hour), streamA),
				event("e2", "T1", model.ActionDone, ago(time.Hour), streamA),
			},
			local: bothLive,
			want:  claims.Unclaimed{},
		},
		{
			name: "leg 1 dropped — the one ticket left is archived out of the flow",
			children: func(t *testing.T) []model.Issue {
				open := leaf(t, "T1", "", model.StateOpen)
				open.SetRetention(model.Archived{})
				return []model.Issue{open}
			},
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionStart, ago(2*time.Hour), streamA),
			},
			local: bothLive,
			want:  claims.Unclaimed{},
		},
		{
			name:     "leg 2 dropped — only non-establishing events, so nobody took the lane",
			children: twoOpen,
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionReopen, ago(3*time.Hour), streamA),
				event("e2", "T1", model.ActionArchive, ago(2*time.Hour), streamA),
				event("e3", "T1", model.ActionClose, ago(time.Hour), streamA),
				// A plain field edit: no verb at all.
				event("e4", "T1", "", ago(time.Minute), streamA),
			},
			local: bothLive,
			want:  claims.Unclaimed{},
		},
		{
			name:     "leg 2 dropped — the latest establishing event carries no attribution",
			children: twoOpen,
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionStart, ago(3*time.Hour), streamA),
				event("e2", "T2", model.ActionStart, ago(time.Hour), model.Attribution{}),
			},
			local: bothLive,
			want:  claims.Unclaimed{},
		},
		{
			name:     "leg 3 dropped — the holder has not touched the lane inside the window",
			children: twoOpen,
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionStart, ago(72*time.Hour), streamA),
				event("e2", "T1", "", ago(48*time.Hour), streamA),
			},
			local: bothLive,
			want:  claims.Stale{Tenure: claims.Tenure{By: streamA, Since: ago(72 * time.Hour), LastActivity: ago(48 * time.Hour)}},
		},
		{
			name:     "leg 4 dropped — this machine has proven the holder's checkout gone",
			children: twoOpen,
			events: []model.IssueEvent{
				event("e1", "T1", model.ActionStart, ago(2*time.Hour), streamA),
			},
			local: claims.NewLocalCheckouts(workspaceID, []string{streamB.Stream()}),
			want:  claims.Unclaimed{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, parents := epicOf(t, tc.children(t)...)
			got := derive(t, issues, parents, tc.events, tc.local).Of(laneIn(epicID, ""))
			assertStanding(t, got, tc.want)
		})
	}
}

// TestUnattributedLatestStopsRatherThanScanning pins the row the migration made
// live: 00005 backfills nothing, so on any repository with history the latest
// establishing event of nearly every lane is unattributed while older, already
// superseded ones may not be. Reading past the unattributed event would hand the
// lane to a checkout that demonstrably moved on.
func TestUnattributedLatestStopsRatherThanScanning(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateClosed), leaf(t, "T2", "", model.StateInProgress))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(3*time.Hour), streamA),
		event("e2", "T1", model.ActionDone, ago(2*time.Hour), streamA),
		// The newest establishing act, by a binary that could not stamp it.
		event("e3", "T2", model.ActionStart, ago(time.Hour), model.Attribution{}),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "")), claims.Unclaimed{})
}

// TestVoidEvidenceFallsThroughToTheNextEstablisher is the counterpart: a locally
// proven-dead checkout's evidence is disproven rather than unknown, so the lane
// reverts to whoever else has standing instead of stopping at the dead one.
func TestVoidEvidenceFallsThroughToTheNextEstablisher(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateOpen), leaf(t, "T2", "", model.StateOpen))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(4*time.Hour), streamB),
		event("e2", "T2", model.ActionStart, ago(time.Hour), streamA),
	}, claims.NewLocalCheckouts(workspaceID, []string{streamB.Stream()}))

	assertStanding(t, standings.Of(laneIn(epicID, "")), held(streamB, ago(4*time.Hour), ago(4*time.Hour)))
}

// TestForeignWorkspaceIsNeverPruned holds the other half of the liveness rule: a
// different clone on this machine carries a different workspace id, and this
// machine's worktree enumeration says nothing about it.
func TestForeignWorkspaceIsNeverPruned(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateOpen))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(time.Hour), foreign),
	}, claims.NewLocalCheckouts(workspaceID, nil))

	assertStanding(t, standings.Of(laneIn(epicID, "")), held(foreign, ago(time.Hour), ago(time.Hour)))
}

// TestAnyMutationRefreshes covers leg 3's other half: ordinary working
// commentary keeps a claim alive through a long stretch on one ticket, even
// though the establishing event itself has aged well out of the window.
func TestAnyMutationRefreshes(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(80*time.Hour), streamA),
		event("e2", "T1", "", ago(30*time.Minute), streamA),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "")), held(streamA, ago(80*time.Hour), ago(30*time.Minute)))
}

// TestCompletingEstablishes pins `done` as an establishing verb: a checkout that
// finished a ticket and is halfway through the lane still holds it.
func TestCompletingEstablishes(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateClosed), leaf(t, "T2", "", model.StateOpen))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionDone, ago(time.Hour), streamA),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "")), held(streamA, ago(time.Hour), ago(time.Hour)))
}

// TestDriveByEditsNeitherEstablishNorContest is the rule the design names
// outright: a comment or a grooming edit from another checkout must not capture
// an epic, and must not raise a contest annotation either.
func TestDriveByEditsNeitherEstablishNorContest(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(3*time.Hour), streamA),
		// B edits a field, archives something, and closes a duplicate — all
		// after A started, and none of it a claim on A's lane.
		event("e2", "T1", "", ago(2*time.Hour), streamB),
		event("e3", "T1", model.ActionArchive, ago(90*time.Minute), streamB),
		event("e4", "T1", model.ActionClose, ago(time.Hour), streamB),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "")), held(streamA, ago(3*time.Hour), ago(3*time.Hour)))
}

// TestContestedAnnotatesWithoutMovingRouting: both checkouts started work in one
// lane, the latest establishing event decides who holds it, and the other side
// is reported rather than hidden.
func TestContestedAnnotatesWithoutMovingRouting(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress), leaf(t, "T2", "", model.StateInProgress))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(3*time.Hour), streamA),
		event("e2", "T2", model.ActionStart, ago(time.Hour), streamB),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "")), claims.Held{
		Tenure:    claims.Tenure{By: streamB, Since: ago(time.Hour), LastActivity: ago(time.Hour)},
		Contested: []model.Attribution{streamA},
	})
}

// TestContestLapsesWithTheRivalsEvidence: a rival whose own evidence has aged
// out is no longer contesting anything.
func TestContestLapsesWithTheRivalsEvidence(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress), leaf(t, "T2", "", model.StateInProgress))
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(200*time.Hour), streamA),
		event("e2", "T2", model.ActionStart, ago(time.Hour), streamB),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "")), held(streamB, ago(time.Hour), ago(time.Hour)))
}

// TestLaneGranularity is the design's reason for choosing the lane over the
// epic: one started ticket in a deliberately multi-lane catch-all epic must not
// monopolize the independent work beside it.
func TestLaneGranularity(t *testing.T) {
	issues, parents := epicOf(t,
		leaf(t, "T1", "bugs", model.StateInProgress),
		leaf(t, "T2", "docs", model.StateOpen),
	)
	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(time.Hour), streamA),
	}, bothLive)

	assertStanding(t, standings.Of(laneIn(epicID, "bugs")), held(streamA, ago(time.Hour), ago(time.Hour)))
	assertStanding(t, standings.Of(laneIn(epicID, "docs")), claims.Unclaimed{})
}

// TestCrossEpicDependencyClaimsOnlyItsOwnLane: pulling a dependency onto a
// stream's path produces evidence in that ticket's lane, never in the
// neighbouring epic that happens to contain it.
func TestCrossEpicDependencyClaimsOnlyItsOwnLane(t *testing.T) {
	dependency := leaf(t, "D1", "", model.StateInProgress)
	neighbour := leaf(t, "D2", "", model.StateOpen)
	other := container(t, "E2", dependency, neighbour)

	mine := leaf(t, "T1", "", model.StateOpen)
	epic := container(t, epicID, mine)

	issues := []model.Issue{epic, other, mine, dependency, neighbour}
	parents := map[string]*model.Issue{mine.ID: &epic, dependency.ID: &other, neighbour.ID: &other}

	standings := derive(t, issues, parents, []model.IssueEvent{
		event("e1", "D1", model.ActionStart, ago(time.Hour), streamA),
	}, bothLive)

	// The dependency's own lane is held...
	assertStanding(t, standings.Of(laneIn("E2", "")), held(streamA, ago(time.Hour), ago(time.Hour)))
	// ...and reaching into E2 claimed nothing in the stream's own epic.
	assertStanding(t, standings.Of(laneIn(epicID, "")), claims.Unclaimed{})
}

// TestParentlessTicketIsItsOwnLane covers the third lane shape.
func TestParentlessTicketIsItsOwnLane(t *testing.T) {
	solo := leaf(t, "S1", "", model.StateInProgress)
	other := leaf(t, "S2", "", model.StateOpen)
	standings := derive(t, []model.Issue{solo, other}, nil, []model.IssueEvent{
		event("e1", "S1", model.ActionStart, ago(time.Hour), streamA),
	}, bothLive)

	assertStanding(t, standings.Of(soloLane("S1")), held(streamA, ago(time.Hour), ago(time.Hour)))
	assertStanding(t, standings.Of(soloLane("S2")), claims.Unclaimed{})
}

// TestColdStartDerivesNothing is the design's graceful-upgrade promise: a
// repository whose whole history predates attribution derives zero claims and
// behaves exactly as it did before.
func TestColdStartDerivesNothing(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress), leaf(t, "T2", "", model.StateOpen))
	events := []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(time.Hour), model.Attribution{}),
		event("e2", "T2", model.ActionDone, ago(30*time.Minute), model.Attribution{}),
	}
	for lane, standing := range derive(t, issues, parents, events, assumeLive) {
		if _, unclaimed := standing.(claims.Unclaimed); !unclaimed {
			t.Fatalf("lane %s on unattributed history = %#v, want Unclaimed", lane, standing)
		}
	}
}

// TestEvidenceRefusesAPartialRead: the completing event that decides a lane's
// holder can sit on a closed ticket, so a caller that filtered its issue list
// must be told rather than handed a plausible "unclaimed".
func TestEvidenceRefusesAPartialRead(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T2", "", model.StateOpen))
	if _, err := claims.NewEvidence(issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionDone, ago(time.Hour), streamA),
	}); err == nil {
		t.Fatal("NewEvidence() with an event about an unsupplied issue = nil error, want a refusal")
	}
}

// TestDeriveIsOrderIndependent: NewEvidence imposes the total order the
// predicate reads, so a caller handing events over shuffled gets the same claims
// as one handing them over sorted.
func TestDeriveIsOrderIndependent(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateInProgress), leaf(t, "T2", "", model.StateOpen))
	ascending := []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(5*time.Hour), streamA),
		event("e2", "T2", model.ActionStart, ago(2*time.Hour), streamB),
		event("e3", "T1", "", ago(time.Hour), streamA),
	}
	shuffled := []model.IssueEvent{ascending[2], ascending[0], ascending[1]}

	lane := laneIn(epicID, "")
	assertStanding(t,
		derive(t, issues, parents, shuffled, bothLive).Of(lane),
		derive(t, issues, parents, ascending, bothLive).Of(lane),
	)
}

// TestDeriveDoesNotConsumeItsEvidence: one reading, derived twice — once by a
// machine that has proven the holder gone and once by a machine that cannot
// check — must give each its own answer. A derivation that pruned in place would
// leave the second call reading a backlog quietly missing the events the first
// one decided to ignore.
func TestDeriveDoesNotConsumeItsEvidence(t *testing.T) {
	issues, parents := epicOf(t, leaf(t, "T1", "", model.StateOpen))
	evidence, err := claims.NewEvidence(issues, parents, []model.IssueEvent{
		event("e1", "T1", model.ActionStart, ago(time.Hour), streamA),
	})
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}

	lane := laneIn(epicID, "")
	pruned := claims.Derive(evidence, fresh, claims.NewLocalCheckouts(workspaceID, nil))
	assertStanding(t, pruned.Of(lane), claims.Unclaimed{})

	// The same reading, asked again by a machine that cannot enumerate.
	assertStanding(t, claims.Derive(evidence, fresh, assumeLive).Of(lane),
		held(streamA, ago(time.Hour), ago(time.Hour)))
}

// TestUnknownLaneReadsAsUnclaimed: Standings.Of is total, so a lookup the
// derivation never saw yields the zero state rather than a nil interface.
func TestUnknownLaneReadsAsUnclaimed(t *testing.T) {
	assertStanding(t, claims.Standings{}.Of(laneIn("nope", "")), claims.Unclaimed{})
}

// assertStanding compares two standings by variant and by every field, which for
// Held includes the contested list. Held carries a slice and so is not
// comparable with ==; the comparison is spelled out rather than reached for
// through reflect, and it never compares an interface directly, which would
// panic the moment a Held turned up on either side.
func assertStanding(t *testing.T, got, want claims.Standing) {
	t.Helper()
	switch expected := want.(type) {
	case claims.Unclaimed:
		if _, ok := got.(claims.Unclaimed); !ok {
			t.Fatalf("standing = %#v, want Unclaimed", got)
		}
	case claims.Stale:
		actual, ok := got.(claims.Stale)
		if !ok {
			t.Fatalf("standing = %#v, want Stale%+v", got, expected.Tenure)
		}
		assertTenure(t, actual.Tenure, expected.Tenure)
	case claims.Held:
		actual, ok := got.(claims.Held)
		if !ok {
			t.Fatalf("standing = %#v, want Held%+v", got, expected.Tenure)
		}
		assertTenure(t, actual.Tenure, expected.Tenure)
		if len(actual.Contested) != len(expected.Contested) {
			t.Fatalf("contested = %v, want %v", actual.Contested, expected.Contested)
		}
		for i := range expected.Contested {
			if actual.Contested[i] != expected.Contested[i] {
				t.Fatalf("contested[%d] = %v, want %v", i, actual.Contested[i], expected.Contested[i])
			}
		}
	default:
		t.Fatalf("unhandled expected standing %#v", want)
	}
}

func assertTenure(t *testing.T, got, want claims.Tenure) {
	t.Helper()
	if got.By != want.By {
		t.Fatalf("holder = %v, want %v", got.By, want.By)
	}
	if !got.Since.Equal(want.Since) {
		t.Fatalf("since = %s, want %s", got.Since, want.Since)
	}
	if !got.LastActivity.Equal(want.LastActivity) {
		t.Fatalf("last activity = %s, want %s", got.LastActivity, want.LastActivity)
	}
}
