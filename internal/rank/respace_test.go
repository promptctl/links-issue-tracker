package rank

import (
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// keyspace runs the store's placement and respace rule over a sorted key list,
// with rank.Respace itself and not a copy of it: bounds read as rankBetweenTx
// reads them, the trigger as smoothRanksIfNeededTx asks it, and the window as
// rankWindowTx cuts it. So a workload here is the rule at any size, in the time
// a Dolt store takes to file a few hundred rows.
type keyspace struct {
	keys []string
	// inserts and rewritten are what rows/insert divides.
	inserts, rewritten int
	// worstRespace and longestWritten read what the rule wrote, never the seed:
	// a store's inherited keys are longer than any bound here.
	worstRespace, longestWritten int
}

func liveKeys(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/live_keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func appendedKeys(t *testing.T, n int) []string {
	t.Helper()
	k := &keyspace{}
	for range n {
		k.insert(t, len(k.keys))
	}
	return k.keys
}

// respace moves the window around trigger, returning the rows it took and the
// rows it changed.
func (k *keyspace) respace(t *testing.T, trigger string) (rows, changed int) {
	t.Helper()
	at := sort.SearchStrings(k.keys, trigger)
	last := len(k.keys) - 1
	first := 0
	keys, whole, err := Respace(trigger, func(n int) (Window, error) {
		from, to := max(at-n, 0), min(at+n, last)
		window := Window{Lower: k.keys[from], Upper: k.keys[to]}
		if from == 0 && to == last {
			window.Whole = len(k.keys)
		}
		if at == 0 {
			from, window.Lower = -1, ""
		}
		if at == last {
			to, window.Upper = last+1, ""
		}
		window.Rows, first = to-from-1, from+1
		return window, nil
	})
	if err != nil {
		t.Fatalf("Respace(%q): %v", trigger, err)
	}
	if whole {
		first = 0
	}
	for i, key := range keys {
		if k.keys[first+i] != key {
			changed++
			k.longestWritten = max(k.longestWritten, len(key))
		}
		k.keys[first+i] = key
	}
	if first > 0 && k.keys[first-1] >= keys[0] {
		t.Fatalf("respace wrote %q at or below the key %q before its window", keys[0], k.keys[first-1])
	}
	if end := first + len(keys); end < len(k.keys) && keys[len(keys)-1] >= k.keys[end] {
		t.Fatalf("respace wrote %q at or above the key %q past its window", keys[len(keys)-1], k.keys[end])
	}
	k.worstRespace = max(k.worstRespace, len(keys))
	return len(keys), changed
}

// insert files one key at index i, respacing where the rule says to, and
// returns the most rows a respace of it took.
func (k *keyspace) insert(t *testing.T, i int) (window int) {
	t.Helper()
	bounds := func() (lower, upper string) {
		if i > 0 {
			lower = k.keys[i-1]
		}
		if i < len(k.keys) {
			upper = k.keys[i]
		}
		return lower, upper
	}
	lower, upper := bounds()
	key, err := Midpoint(lower, upper)
	if errors.Is(err, ErrNoRoom) {
		trigger := upper
		if trigger == "" {
			trigger = lower
		}
		rows, changed := k.respace(t, trigger)
		window = rows
		k.rewritten += changed
		lower, upper = bounds()
		key, err = Midpoint(lower, upper)
	}
	if err != nil {
		t.Fatalf("insert %d at %d: %v", k.inserts, i, err)
	}
	if lower != "" && key <= lower || upper != "" && key >= upper {
		t.Fatalf("insert %d at %d: %q does not sort between %q and %q", k.inserts, i, key, lower, upper)
	}
	k.keys = slices.Insert(k.keys, i, key)
	k.longestWritten = max(k.longestWritten, len(key))
	// A neighbour at the edge never moves, so it says nothing about density.
	var movable []string
	if i > 1 {
		movable = append(movable, lower)
	}
	if i < len(k.keys)-2 {
		movable = append(movable, upper)
	}
	if Crowded(key, movable...) {
		rows, changed := k.respace(t, key)
		window = max(window, rows)
		k.rewritten += changed
	}
	k.inserts++
	return window
}

// settled fails unless the keys ascend strictly and none tie on the index
// prefix, and unless nothing written passed MaxLength.
func (k *keyspace) settled(t *testing.T, label string) {
	t.Helper()
	for i := 1; i < len(k.keys); i++ {
		if k.keys[i] <= k.keys[i-1] {
			t.Fatalf("%s: keys not strictly ascending at %d", label, i)
		}
	}
	if tied := PrefixTied(k.keys); tied != 0 {
		t.Fatalf("%s: %d adjacent keys tie on the index prefix", label, tied)
	}
	if k.longestWritten > MaxLength {
		t.Fatalf("%s: wrote a key of %d chars", label, k.longestWritten)
	}
}

func from(seed []string) *keyspace {
	return &keyspace{keys: slices.Clone(seed)}
}

func bottomOf(k *keyspace) int { return len(k.keys) }

func topOf(*keyspace) int { return 0 }

// mixed files four in ten at the bottom, one at the top, and the rest anywhere.
func mixed(seed int64) func(*keyspace) int {
	rng := rand.New(rand.NewSource(seed))
	return func(k *keyspace) int {
		switch r := rng.Intn(10); {
		case r < 4:
			return len(k.keys)
		case r < 5:
			return 0
		default:
			return rng.Intn(len(k.keys) + 1)
		}
	}
}

// litLike files as this store files: nine in ten at the bottom, and most of the
// rest near it.
func litLike(seed int64) func(*keyspace) int {
	rng := rand.New(rand.NewSource(seed))
	return func(k *keyspace) int {
		switch r := rng.Intn(100); {
		case r < 90:
			return len(k.keys)
		case r < 95:
			return 0
		default:
			return len(k.keys) - rng.Intn(min(len(k.keys), 50))
		}
	}
}

// Realistic work on this store's own keys stays local: a respace is rare, small,
// and leaves the keys it touches short. Keys only, no ids, in testdata.
func TestRealisticWorkloadsRespaceLittle(t *testing.T) {
	if testing.Short() {
		t.Skip("a million placements across the workloads")
	}
	live := liveKeys(t)
	moved := from(live)
	moved.insert(t, 625)
	for _, w := range []struct {
		name string
		seed []string
		ops  int
		at   func(*keyspace) int
	}{
		{"appends", live, 300000, bottomOf},
		{"prepends", live, 100000, topOf},
		{"mixed", live, 100000, mixed(2)},
		{"lit-like 1", live, 100000, litLike(1)},
		{"lit-like 2", live, 100000, litLike(2)},
		{"lit-like 3", live, 100000, litLike(3)},
		{"a move, then appends", moved.keys, 300000, bottomOf},
	} {
		t.Run(w.name, func(t *testing.T) {
			k := from(w.seed)
			for range w.ops {
				k.insert(t, w.at(k))
			}
			k.settled(t, w.name)
			if k.worstRespace > 64 {
				t.Errorf("worst respace %d rows, want at most 64", k.worstRespace)
			}
		})
	}
	t.Run("frame-top mix", func(t *testing.T) {
		k := from(live)
		// Containers are rows: a respace changes their keys and not their places.
		hot := []int{50, len(k.keys) / 2, len(k.keys) - 20}
		rng := rand.New(rand.NewSource(7))
		for range 30000 {
			i := len(k.keys)
			if rng.Intn(10) < 3 {
				i = hot[rng.Intn(len(hot))] + 1
			}
			k.insert(t, i)
			for j, row := range hot {
				if i <= row {
					hot[j] = row + 1
				}
			}
		}
		k.settled(t, "frame-top mix")
		if k.worstRespace > 64 {
			t.Errorf("worst respace %d rows, want at most 64", k.worstRespace)
		}
	})
}

// A spot filed over and over pays for it in key length past the cap and never
// in window size: the bound the design commits to, at every spot the live keys
// and an empty store offer, the edges included, where an immovable neighbour
// once respaced nearly every insert.
func TestSameSpotInsertsStayBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("20,000 placements at each of 14 spots")
	}
	live := liveKeys(t)
	empty := appendedKeys(t, 200)
	type spot struct {
		name string
		seed []string
		at   int
	}
	var spots []spot
	for _, at := range []int{1, 2, 3, 200, 625, 1000, 1129, len(live) - 2, len(live) - 1} {
		spots = append(spots, spot{fmt.Sprintf("live @%d", at), live, at})
	}
	for _, at := range []int{1, 2, 101, 198} {
		spots = append(spots, spot{fmt.Sprintf("empty @%d", at), empty, at})
	}
	for _, s := range spots {
		t.Run(s.name, func(t *testing.T) {
			k := from(s.seed)
			for range 20000 {
				k.insert(t, s.at)
			}
			k.settled(t, s.name)
			if k.worstRespace > MaxRespace {
				t.Errorf("worst respace %d rows, want at most %d", k.worstRespace, MaxRespace)
			}
			if k.longestWritten > 70 {
				t.Errorf("longest written key %d chars, want at most 70", k.longestWritten)
			}
			if perInsert := float64(k.rewritten) / float64(k.inserts); perInsert > 45 {
				t.Errorf("%.1f rows/insert, want at most 45", perInsert)
			}
		})
	}
}

// At the limit cost rises and order holds. The whole run to it takes 241,000
// inserts, so this starts there: a thousand keys one unit apart, sharing 186
// chars, where the only room is past the cap.
func TestAWindowPassesTheCapOnlyWhereNoRoomIsInside(t *testing.T) {
	prefix := strings.Repeat("a", 186)
	var seed []string
	for i := range 20 {
		seed = append(seed, fmt.Sprintf("A%03d", i))
	}
	for i := range 1000 {
		suffix, err := encodeBase62(big.NewInt(int64(100000+i)), 4)
		if err != nil {
			t.Fatal(err)
		}
		seed = append(seed, prefix+suffix)
	}
	for i := range 20 {
		seed = append(seed, fmt.Sprintf("b%03d", i))
	}
	k := from(seed)
	spot := 520
	for range 3000 {
		k.insert(t, spot)
	}
	k.settled(t, "the limit")
	if k.worstRespace <= MaxRespace {
		t.Errorf("worst respace %d rows, want a window past %d: the seed has no room inside the cap", k.worstRespace, MaxRespace)
	}
}

// A corner too deep to step out of is no room at all: a window that stayed in
// it spaced its keys and refused the next placement anyway.
func TestARespaceInACornerLeavesRoomForTheNextPlacement(t *testing.T) {
	for _, c := range []struct {
		name  string
		char  string
		place func(*keyspace) int
	}{
		{"past z", "z", bottomOf},
		{"before 0", "0", topOf},
	} {
		for _, rows := range []int{40, 2000} {
			t.Run(fmt.Sprintf("%s, %d rows", c.name, rows), func(t *testing.T) {
				corner := strings.Repeat(c.char, 128)
				keys := make([]string, rows)
				for i := range keys {
					keys[i] = fmt.Sprintf("%s1%03X", corner, i)
				}
				k := from(keys)
				at := c.place(k)
				if _, err := step(keys[min(at, rows-1)], map[string]int64{"z": 1, "0": -1}[c.char]); !errors.Is(err, ErrNoRoom) {
					t.Fatalf("step out of the corner: %v, want ErrNoRoom: the seed is not a corner", err)
				}
				k.insert(t, at)
				k.insert(t, c.place(k))
				k.settled(t, c.name)
			})
		}
	}
}

// Two clones that have not synced, each filing one row, merged as sync merges
// rank, per issue: a side that changed a key wins, and when both did the tiebreak
// picks one. It returns how far row B lands from where B meant it.
func displacedAfterMerge(t *testing.T, base []string, aAt, bAt int, bWins bool) int {
	t.Helper()
	side := func(at int) (keys []string, filed string) {
		k := from(base)
		k.insert(t, at)
		filed = k.keys[at]
		return slices.Delete(k.keys, at, at+1), filed
	}
	a, aFiled := side(aAt)
	b, bFiled := side(bAt)
	const aRow, bRow = -1, -2
	type row struct {
		id  int
		key string
	}
	merged := []row{{aRow, aFiled}, {bRow, bFiled}}
	for i := range base {
		key := base[i]
		switch aMoved, bMoved := a[i] != base[i], b[i] != base[i]; {
		case aMoved && bMoved && !bWins, aMoved && !bMoved:
			key = a[i]
		case bMoved:
			key = b[i]
		}
		merged = append(merged, row{i, key})
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].key < merged[j].key })
	landed := slices.IndexFunc(merged, func(r row) bool { return r.id == bRow })
	// Where B meant it: above the row it was filed above, with A's row in front
	// of B's when A filed earlier in the list.
	intended := bAt
	if aAt <= bAt {
		intended++
	}
	return max(landed-intended, intended-landed)
}

// A respace two clones race reorders only what it moved: a row filed inside the
// window lands no further off than the window is wide, and a row filed outside
// it lands exactly where it was filed.
func TestACloneRacingARespaceIsDisplacedOnlyInsideTheWindow(t *testing.T) {
	k := from(appendedKeys(t, 200))
	const spot = 101
	var base []string
	widest := 0
	for range 20000 {
		before := slices.Clone(k.keys)
		if window := k.insert(t, spot); window > widest {
			widest, base = window, before
		}
	}
	if widest < 64 {
		t.Fatalf("widest respace %d rows, want a wide one to race", widest)
	}
	for _, bWins := range []bool{true, false} {
		if off := displacedAfterMerge(t, base, spot, spot+widest/2, bWins); off > widest {
			t.Errorf("filed inside a %d-row window, bWins=%t: displaced %d", widest, bWins, off)
		}
		for _, outside := range []int{spot + 2*widest, spot + 4*widest} {
			if off := displacedAfterMerge(t, base, spot, outside, bWins); off != 0 {
				t.Errorf("filed outside a %d-row window at %d, bWins=%t: displaced %d", widest, outside, bWins, off)
			}
		}
	}
}
