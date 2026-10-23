package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
)

// Ten times on both axes, from the one census: the store is ten times the
// largest store counted, and the fleet is every store counted, ten times.
func TestTheTenfoldScalePointsAreTenTimesTheCensus(t *testing.T) {
	if want := 10 * slices.Max(fleetCensus); tenfoldStore.rows != want || len(tenfoldStore.beside) != 0 {
		t.Errorf("10x-store = %d rows beside %d stores, want one store of %d", tenfoldStore.rows, len(tenfoldStore.beside), want)
	}
	members := tenfoldFleet.members()
	if len(members) != 10*len(fleetCensus) {
		t.Fatalf("10x-fleet holds %d stores, want %d", len(members), 10*len(fleetCensus))
	}
	want := slices.Concat(slices.Repeat([][]int{fleetCensus}, 10)...)
	slices.Sort(want)
	got := slices.Clone(members)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("10x-fleet's stores are not the census ten times over")
	}
	if members[0] != slices.Max(fleetCensus) {
		t.Errorf("10x-fleet measures a %d-row store, want its largest, %d", members[0], slices.Max(fleetCensus))
	}
}

// The storm writes to the first stormWriters members, so they must be the
// fleet's shape rather than its ten largest stores ten times each.
func TestTheStormedMembersOfTheFleetCoverEveryCensusSize(t *testing.T) {
	stormed := map[int]int{}
	for _, rows := range tenfoldFleet.members()[:stormWriters] {
		stormed[rows]++
	}
	for _, rows := range fleetCensus {
		if stormed[rows] < 3 {
			t.Errorf("the storm writes to %d store(s) of %d rows, want every census size at least three times", stormed[rows], rows)
		}
	}
}

func TestParseSizesTakesEveryNamedScalePoint(t *testing.T) {
	for _, named := range namedSizes {
		got, err := parseSizes(named.name)
		if err != nil || len(got) != 1 || got[0].name != named.name || got[0].rows != named.rows || len(got[0].beside) != len(named.beside) {
			t.Errorf("parseSizes(%q) = (%v, %v), want the named scale point", named.name, got, err)
		}
	}
	if _, err := parseSizes("11x-store"); err == nil || !strings.Contains(err.Error(), `"10x-fleet"`) {
		t.Errorf("parseSizes of an unknown name = %v, want an error listing the names it takes", err)
	}
}

// A limit key is the label and the topic of the ticket the nightly lane files,
// so lit must take it as a topic unchanged, and two limits must never share
// one: their nights would land on one ticket.
func TestEveryLimitKeyIsADistinctLitTopic(t *testing.T) {
	seen := map[string]string{}
	for _, sz := range namedSizes {
		var whats []string
		for _, p := range probes {
			whats = append(whats, p.name)
		}
		for _, in := range ceilingInputs {
			whats = append(whats, in.name)
		}
		whats = append(whats, "storm")
		for _, what := range whats {
			key := limitKey(sz, what)
			topic, err := issueid.NormalizeTopicForCreate(key)
			if err != nil || topic != key {
				t.Errorf("limit key %q (%s, %s) is not a lit topic as written: (%q, %v)", key, sz.name, what, topic, err)
			}
			if other, dup := seen[key]; dup {
				t.Errorf("limit key %q names both %s and %s/%s", key, other, sz.name, what)
			}
			seen[key] = sz.name + "/" + what
		}
	}
}

// Writer i writes to member i mod N: against one store all hundred land on
// it, and against a fleet each lands on its own store.
func TestTheStormSpreadsItsWritersAcrossTheMembers(t *testing.T) {
	members := []generatedStore{{root: "/f/store-000"}, {root: "/f/store-001"}, {root: "/f/store-002"}}
	var (
		mu    sync.Mutex
		wrote = map[string]int{}
	)
	run := func(dir string, env []string, st step) ([]byte, error) {
		mu.Lock()
		wrote[dir]++
		mu.Unlock()
		return nil, nil
	}
	storm(litBinary{path: "lit"}, members, latencyBudget, run)
	if wrote["/f/store-000"] != 34 || wrote["/f/store-001"] != 33 || wrote["/f/store-002"] != 33 {
		t.Errorf("writers per member = %v, want 34/33/33", wrote)
	}
}

// The fleet probes name the fleet root; every other probe names nothing
// outside its store.
func TestTheFleetProbesNameTheFleetRoot(t *testing.T) {
	store := generatedStore{root: "/f/store-000", fleetRoot: "/f", ids: []string{"bench-a"}}
	for _, inv := range invocationsFor(probes, store) {
		over := strings.HasPrefix(inv.probe.name, "stores")
		if names := slices.Contains(inv.args, store.fleetRoot); names != over {
			t.Errorf("probe %q runs %v; naming the fleet root is %v, want %v", inv.probe.name, inv.args, names, over)
		}
	}
}

func TestAFleetNamesItsStoreCount(t *testing.T) {
	if got := (size{name: "n=5", rows: 5}).String(); got != "n=5 (5 rows)" {
		t.Errorf("single store = %q", got)
	}
	if got := (size{name: "f", rows: 9, beside: []int{1, 2}}).String(); got != "f (9 rows, 1 of 3 stores)" {
		t.Errorf("fleet = %q", got)
	}
}

// --limits holds this run's verdicts alone: a file left by an earlier run is
// emptied, or the lane would file its limits again as tonight's.
func TestTheLimitsFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.jsonl")
	if err := os.WriteFile(path, []byte(`{"limit":"stale"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, done, err := limitsSink(path)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if got, _ := os.ReadFile(path); len(got) != 0 {
		t.Errorf("--limits kept an earlier run's records: %q", got)
	}
}
