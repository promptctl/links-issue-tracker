package rank

import (
	"errors"
	"math/big"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

// openingKey is the midpoint of the whole keyspace, the alphabet's middle.
const openingKey = "V"

func TestValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},        // empty is "unranked", not a stored rank
		{openingKey, true}, // a workspace's first key
		{"V", true},        // single alphabet char
		{"0z", true},       // multi-byte, alphabet bounds
		{"aZ09", true},     // mixed multi-byte
		{"-", false},       // outside the alphabet
		{"V!", false},      // one bad byte among good
		{" ", false},       // space is not in the alphabet
		{"hello world", false},
	}
	for _, c := range cases {
		if got := Valid(c.in); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Ranks that pad to the same value share a significant part.
func TestSignificant(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"V", "V"},
		{"V0", "V"},
		{"V00", "V"},
		{"V0z", "V0z"}, // an inner zero is significant
		{"100", "1"},
		{"0", ""}, // an all-zero rank has no significant part
		{"", ""},
	} {
		if got := Significant(c.in); got != c.want {
			t.Errorf("Significant(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMidpointBasic(t *testing.T) {
	tests := []struct {
		a, b string
	}{
		{"A", "z"},
		{"A", "B"},
		{"0", "z"},
		{"V", "X"},
		{"aaa", "aac"},
	}
	for _, tt := range tests {
		mid, err := Midpoint(tt.a, tt.b)
		if err != nil {
			t.Errorf("Midpoint(%q, %q) error: %v", tt.a, tt.b, err)
			continue
		}
		if mid <= tt.a || mid >= tt.b {
			t.Errorf("Midpoint(%q, %q) = %q, want %q < result < %q", tt.a, tt.b, mid, tt.a, tt.b)
		}
	}
}

func TestMidpointAdjacentChars(t *testing.T) {
	// Adjacent characters in alphabet: must extend to next position.
	mid, err := Midpoint("A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if mid <= "A" || mid >= "B" {
		t.Errorf("Midpoint(A, B) = %q, want A < result < B", mid)
	}
	if len(mid) <= 1 {
		t.Errorf("expected midpoint between adjacent chars to be longer, got %q", mid)
	}
}

func TestMidpointErrors(t *testing.T) {
	_, err := Midpoint("B", "A")
	if err == nil {
		t.Error("expected error for a >= b")
	}
	_, err = Midpoint("A", "A")
	if err == nil {
		t.Error("expected error for a == b")
	}
}

// Every pair of strings up to four characters over an alphabet of the floor,
// the ceiling, and one character beside each. The small alphabet is what makes
// the pairs this primitive gets wrong common rather than rare: one bound a
// prefix of the other, one the other extended by zeros, an all-zero bound. The
// empty string plays both open ends.
//
// Midpoint must refuse exactly the pairs sharing a significant part, with
// ErrNoRoom, and on every other pair return a valid rank strictly between the
// bounds.
func TestMidpointStaysStrictlyBetweenItsBounds(t *testing.T) {
	t.Parallel()
	strs := []string{""}
	for frontier := []string{""}; len(frontier[0]) < 4; {
		var next []string
		for _, s := range frontier {
			for _, c := range "01yz" {
				next = append(next, s+string(c))
			}
		}
		strs = append(strs, next...)
		frontier = next
	}
	pairs := 0
	for _, a := range strs {
		for _, b := range strs {
			if (a == b && a != "") || (b != "" && a >= b) {
				continue
			}
			pairs++
			got, err := Midpoint(a, b)
			if b != "" && Significant(a) == Significant(b) {
				if !errors.Is(err, ErrNoRoom) {
					t.Fatalf("Midpoint(%q, %q) = %q, %v; want ErrNoRoom: the bounds pad to the same value", a, b, got, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("Midpoint(%q, %q) error: %v; the bounds have room", a, b, err)
			}
			if !Valid(got) || got <= a || (b != "" && got >= b) {
				t.Fatalf("Midpoint(%q, %q) = %q, want a valid rank strictly between them", a, b, got)
			}
		}
	}
	// A generator that stopped generating would pass every assertion above. Each
	// unordered pair of distinct strings is checked once, plus every string
	// against the open upper end, the two open ends together included.
	if n := len(strs); pairs != n*(n-1)/2+n {
		t.Fatalf("checked %d pairs of %d strings, want %d", pairs, n, n*(n-1)/2+n)
	}
}

// Nothing but the empty string, which means unranked, sorts below an all-zero
// rank, so a placement above it has nothing to return.
func TestNothingSortsBelowAnAllZeroRank(t *testing.T) {
	for _, a := range []string{"0", "000"} {
		if got, err := Midpoint("", a); !errors.Is(err, ErrNoRoom) {
			t.Errorf("Midpoint(\"\", %q) = %q, %v; want ErrNoRoom", a, got, err)
		}
	}
}

func TestMidpointEmptyBounds(t *testing.T) {
	// Empty a = before everything.
	mid, err := Midpoint("", "V")
	if err != nil {
		t.Fatal(err)
	}
	if mid >= "V" {
		t.Errorf("Midpoint('', 'V') = %q, want < V", mid)
	}
	if mid == "" {
		t.Error("Midpoint('', 'V') returned empty string")
	}

	// Empty b = after everything.
	mid, err = Midpoint("V", "")
	if err != nil {
		t.Fatal(err)
	}
	if mid <= "V" {
		t.Errorf("Midpoint('V', '') = %q, want > V", mid)
	}
}

func TestSequentialAppends(t *testing.T) {
	// Create 1000 items past the open end — verify strictly increasing.
	ranks := make([]string, 1001)
	ranks[0] = openingKey
	for i := 1; i <= 1000; i++ {
		r, err := Midpoint(ranks[i-1], "")
		if err != nil {
			t.Fatalf("Midpoint(%q, \"\") at step %d error: %v", ranks[i-1], i, err)
		}
		ranks[i] = r
		if ranks[i] <= ranks[i-1] {
			t.Fatalf("Midpoint(%q, \"\") = %q at step %d, not strictly increasing", ranks[i-1], ranks[i], i)
		}
	}
}

func TestSequentialPrepends(t *testing.T) {
	// Create 1000 items past the other open end — verify strictly decreasing.
	ranks := make([]string, 1001)
	ranks[0] = openingKey
	for i := 1; i <= 1000; i++ {
		r, err := Midpoint("", ranks[i-1])
		if err != nil {
			t.Fatalf("Midpoint(\"\", %q) at step %d error: %v", ranks[i-1], i, err)
		}
		ranks[i] = r
		if ranks[i] >= ranks[i-1] {
			t.Fatalf("Midpoint(\"\", %q) = %q at step %d, not strictly decreasing", ranks[i-1], ranks[i], i)
		}
	}
}

func TestRepeatedMidpointInsertion(t *testing.T) {
	// Repeatedly insert at the bottom of the same gap, the worst case for string
	// growth: each key sorts between its bounds until the length limit refuses.
	lo := "A"
	hi := "B"
	for i := 0; ; i++ {
		mid, err := Midpoint(lo, hi)
		if errors.Is(err, ErrNoRoom) {
			if i < 900 {
				t.Fatalf("step %d: Midpoint(%q, %q) refused long before MaxLength", i, lo, hi)
			}
			return
		}
		if err != nil {
			t.Fatalf("step %d: Midpoint(%q, %q) error: %v", i, lo, hi, err)
		}
		if mid <= lo || mid >= hi || len(mid) > MaxLength {
			t.Fatalf("step %d: Midpoint(%q, %q) = %q, ordering or length violated", i, lo, hi, mid)
		}
		hi = mid
	}
}

func TestMidpointMultiCharStrings(t *testing.T) {
	tests := []struct {
		a, b string
	}{
		{"V0", "V2"},
		{"aV", "aX"},
		{"VVV", "VVX"},
		{"abc", "abd"},
	}
	for _, tt := range tests {
		mid, err := Midpoint(tt.a, tt.b)
		if err != nil {
			t.Errorf("Midpoint(%q, %q) error: %v", tt.a, tt.b, err)
			continue
		}
		if mid <= tt.a || mid >= tt.b {
			t.Errorf("Midpoint(%q, %q) = %q, ordering violated", tt.a, tt.b, mid)
		}
	}
}

func TestBuildSequenceWithMidpoints(t *testing.T) {
	// Build a sequence: initial, then insert between each adjacent pair.
	// Simulates rank-above/rank-below operations.
	ranks := []string{openingKey}
	for i := 0; i < 5; i++ {
		next, err := Midpoint(ranks[len(ranks)-1], "")
		if err != nil {
			t.Fatalf("Midpoint(%q, \"\") error: %v", ranks[len(ranks)-1], err)
		}
		ranks = append(ranks, next)
	}
	// Now insert between each adjacent pair.
	var expanded []string
	expanded = append(expanded, ranks[0])
	for i := 1; i < len(ranks); i++ {
		mid, err := Midpoint(ranks[i-1], ranks[i])
		if err != nil {
			t.Fatalf("Midpoint(%q, %q) error: %v", ranks[i-1], ranks[i], err)
		}
		expanded = append(expanded, mid, ranks[i])
	}
	// Verify strict ordering.
	for i := 1; i < len(expanded); i++ {
		if expanded[i] <= expanded[i-1] {
			t.Errorf("ordering violated at index %d: %q <= %q", i, expanded[i], expanded[i-1])
		}
	}
}

func TestSpacedRanksOrdering(t *testing.T) {
	for _, n := range []int{1, 2, 10, 100, 1000, 15000} {
		ranks := SpacedRanks(n)
		if len(ranks) != n {
			t.Fatalf("SpacedRanks(%d) returned %d items", n, len(ranks))
		}
		for i := 1; i < len(ranks); i++ {
			if ranks[i] <= ranks[i-1] {
				t.Fatalf("SpacedRanks(%d): ordering violated at index %d: %q <= %q", n, i, ranks[i], ranks[i-1])
			}
		}
	}
}

func TestSpacedRanksUniformLength(t *testing.T) {
	ranks := SpacedRanks(15000)
	length := len(ranks[0])
	for i, r := range ranks {
		if len(r) != length {
			t.Fatalf("SpacedRanks(15000)[%d] has length %d, expected %d", i, len(r), length)
		}
	}
	t.Logf("SpacedRanks(15000): length=%d", length)
}

func TestSpacedRanksAllowMidpointInsertion(t *testing.T) {
	// After rebalancing, we should be able to insert between every adjacent pair.
	ranks := SpacedRanks(100)
	for i := 1; i < len(ranks); i++ {
		mid, err := Midpoint(ranks[i-1], ranks[i])
		if err != nil {
			t.Fatalf("Midpoint(%q, %q) after rebalance: %v", ranks[i-1], ranks[i], err)
		}
		if mid <= ranks[i-1] || mid >= ranks[i] {
			t.Fatalf("Midpoint(%q, %q) = %q, ordering violated", ranks[i-1], ranks[i], mid)
		}
	}
}

func TestSpacedRanksEmpty(t *testing.T) {
	ranks := SpacedRanks(0)
	if len(ranks) != 0 {
		t.Fatalf("SpacedRanks(0) returned %d items", len(ranks))
	}
}

func TestSpacedRanksBetween(t *testing.T) {
	// Generate 32 ranks between two boundaries (typical smoothing scenario).
	ranks, err := SpacedRanksBetween("B", "F", 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranks) != 32 {
		t.Fatalf("got %d ranks, want 32", len(ranks))
	}
	// All ranks must be strictly between B and F.
	for i, r := range ranks {
		if r <= "B" || r >= "F" {
			t.Fatalf("rank[%d] = %q, not between B and F", i, r)
		}
	}
	// Strictly increasing.
	for i := 1; i < len(ranks); i++ {
		if ranks[i] <= ranks[i-1] {
			t.Fatalf("ordering violated at %d: %q <= %q", i, ranks[i], ranks[i-1])
		}
	}
}

func TestSpacedRanksBetweenEdges(t *testing.T) {
	// No lower bound (smoothing at the front of the list).
	ranks, err := SpacedRanksBetween("", "D", 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ranks {
		if r >= "D" {
			t.Fatalf("rank %q not below upper bound D", r)
		}
	}
	for i := 1; i < len(ranks); i++ {
		if ranks[i] <= ranks[i-1] {
			t.Fatalf("ordering violated: %q <= %q", ranks[i], ranks[i-1])
		}
	}

	// No upper bound (smoothing at the back of the list).
	ranks, err = SpacedRanksBetween("w", "", 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ranks {
		if r <= "w" {
			t.Fatalf("rank %q not above lower bound w", r)
		}
	}
	for i := 1; i < len(ranks); i++ {
		if ranks[i] <= ranks[i-1] {
			t.Fatalf("ordering violated: %q <= %q", ranks[i], ranks[i-1])
		}
	}
}

func TestSpacedRanksBetweenAllowsFurtherMidpoints(t *testing.T) {
	// After smoothing, we should be able to insert between every adjacent pair.
	ranks, err := SpacedRanksBetween("C", "G", 32)
	if err != nil {
		t.Fatal(err)
	}
	// Insert between C and ranks[0].
	mid, err := Midpoint("C", ranks[0])
	if err != nil {
		t.Fatalf("Midpoint(C, %q): %v", ranks[0], err)
	}
	if mid <= "C" || mid >= ranks[0] {
		t.Fatalf("boundary midpoint failed: %q", mid)
	}
	// Insert between each adjacent pair.
	for i := 1; i < len(ranks); i++ {
		mid, err := Midpoint(ranks[i-1], ranks[i])
		if err != nil {
			t.Fatalf("Midpoint(%q, %q): %v", ranks[i-1], ranks[i], err)
		}
		if mid <= ranks[i-1] || mid >= ranks[i] {
			t.Fatalf("midpoint ordering violated")
		}
	}
}

func TestSpacedRanksBetweenLongLowerBound(t *testing.T) {
	// [LAW:dataflow-not-control-flow] Long bounds are valid input data; spacing must adapt without control-flow panics.
	lower := strings.Repeat("z", 11)
	ranks, err := SpacedRanksBetween(lower, "", 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranks) != 32 {
		t.Fatalf("got %d ranks, want 32", len(ranks))
	}
	for i, r := range ranks {
		if r <= lower {
			t.Fatalf("rank[%d] = %q, not above lower bound %q", i, r, lower)
		}
	}
	for i := 1; i < len(ranks); i++ {
		if ranks[i] <= ranks[i-1] {
			t.Fatalf("ordering violated at %d: %q <= %q", i, ranks[i], ranks[i-1])
		}
	}
}

func TestSpacedRanksBetweenRejectsNegativeN(t *testing.T) {
	_, err := SpacedRanksBetween("", "", -1)
	if err == nil {
		t.Fatal("expected error for negative n")
	}
	if !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("error = %q, want non-negative validation", err)
	}
}

func TestSpacedRanksBetweenRejectsInvertedBoundsForAnyN(t *testing.T) {
	for _, n := range []int{0, 1} {
		if ranks, err := SpacedRanksBetween("Z", "A", n); err == nil {
			t.Fatalf("SpacedRanksBetween(%q, %q, %d) = %q, want an error: the bounds are inverted however many ranks are asked for", "Z", "A", n, ranks)
		}
	}
}

func TestSpacedRanksPanicsOnNegativeN(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for negative n")
		}
	}()
	_ = SpacedRanks(-1)
}

// Bounds with no room are two adjacent stored ranks that pad to the same value
// — one is the other extended by zeros. The pair is representable in a real
// store (54 of the 683 ranks in this repo's own store end in '0'), and both
// callers of this primitive, the doctor repair and the smoothing pass, take
// their bounds straight from stored ranks, so the primitive must report such a
// pair rather than search forever.
//
// The goroutine IS the assertion. A regression here is a hang, and a hang left
// to the package timeout burns a CI runner for ten minutes before naming
// anything.
func TestSpacedRanksBetweenRejectsBoundsWithNoRoom(t *testing.T) {
	t.Parallel()
	for _, bounds := range []struct{ lower, upper string }{
		{"10", "100"},    // upper is lower plus one zero: nothing sorts between at all
		{"1", "100"},     // "10" sorts between, but no rank longer than both does
		{"0V", "0V0000"}, // the shape a spaced rank and its zero-extension make
		{"", "0"},        // an all-zero upper bound: nothing sorts below it
	} {
		t.Run(bounds.lower+"_"+bounds.upper, func(t *testing.T) {
			t.Parallel()
			type outcome struct {
				ranks []string
				err   error
			}
			returned := make(chan outcome, 1)
			go func() {
				ranks, err := SpacedRanksBetween(bounds.lower, bounds.upper, 1)
				returned <- outcome{ranks, err}
			}()
			select {
			case got := <-returned:
				if !errors.Is(got.err, ErrNoRoom) {
					t.Fatalf("SpacedRanksBetween(%q, %q, 1) = %q, %v; want ErrNoRoom: no rank longer than both bounds sorts between them",
						bounds.lower, bounds.upper, got.ranks, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("SpacedRanksBetween(%q, %q, 1) did not return within 5s — the length search is looping on bounds it can never satisfy",
					bounds.lower, bounds.upper)
			}
		})
	}
}

// A workspace with nothing ranked has two open ends, and the store seeds it
// with the midpoint of that pair.
func TestMidpointOfTheWholeKeyspaceIsTheOpeningKey(t *testing.T) {
	t.Parallel()
	if got, err := Midpoint("", ""); err != nil || got != openingKey {
		t.Fatalf("Midpoint(\"\", \"\") = %q, %v; want %q", got, err, openingKey)
	}
}

// A key costs what its bounds' room costs and no more: bisecting past the
// character where the bounds differ is what grew keys to 648 chars.
func TestMidpointIsNoLongerThanItsBoundsNeed(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	word := func() string {
		out := make([]byte, 1+rng.Intn(12))
		for i := range out {
			out[i] = alphabet[rng.Intn(base)]
		}
		return string(out)
	}
	four := big.NewInt(4)
	for range 20000 {
		a, b := word(), word()
		if a > b {
			a, b = b, a
		}
		if Significant(a) == Significant(b) {
			continue
		}
		got, err := Midpoint(a, b)
		if err != nil {
			t.Fatalf("Midpoint(%q, %q): %v", a, b, err)
		}
		if got <= a || got >= b {
			t.Fatalf("Midpoint(%q, %q) = %q, not between them", a, b, got)
		}
		roomy := 1
		for ; ; roomy++ {
			lo, err := stringToInt(a, roomy)
			if err != nil {
				t.Fatal(err)
			}
			hi, err := stringToInt(b, roomy)
			if err != nil {
				t.Fatal(err)
			}
			if new(big.Int).Sub(hi, lo).Cmp(four) >= 0 {
				break
			}
		}
		if len(got) > roomy {
			t.Fatalf("Midpoint(%q, %q) = %q, %d chars, and the bounds sit 4 apart at %d", a, b, got, len(got), roomy)
		}
	}
}

func TestAGapOfFourKeepsItsLength(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ a, b, want string }{
		{"aaa0", "aaa4", "aaa2"},
		{"aaa0", "aaa3", "aaa1U"},
	} {
		if got, err := Midpoint(c.a, c.b); err != nil || got != c.want {
			t.Errorf("Midpoint(%q, %q) = %q, %v; want %q", c.a, c.b, got, err, c.want)
		}
	}
}

// Past MaxLength every primitive says no room, the answer its callers already
// act on, so no write can leave a key the index prefix would tie.
func TestNoPrimitiveWritesPastMaxLength(t *testing.T) {
	t.Parallel()
	shared := strings.Repeat("a", MaxLength)
	if got, err := Midpoint(shared+"1", shared+"3"); !errors.Is(err, ErrNoRoom) {
		t.Errorf("Midpoint inside the limit's last char = %q, %v; want ErrNoRoom", got, err)
	}
	if got, err := SpacedRanksBetween(shared+"1", shared+"3", 1); !errors.Is(err, ErrNoRoom) {
		t.Errorf("SpacedRanksBetween inside the limit's last char = %q, %v; want ErrNoRoom", got, err)
	}
	if got, err := Midpoint(strings.Repeat("z", MaxLength), ""); !errors.Is(err, ErrNoRoom) {
		t.Errorf("a step past z^%d = %q, %v; want ErrNoRoom", MaxLength, got, err)
	}
	if got, err := Midpoint("", strings.Repeat("0", MaxLength)+"1"); !errors.Is(err, ErrNoRoom) {
		t.Errorf("a step below a key past 0^%d = %q, %v; want ErrNoRoom", MaxLength, got, err)
	}
}

// A step is one unit at the shortest power-of-two length from 4 that admits one,
// so appends take no room from the keys they follow, and a corner costs the log
// of its depth.
func TestAPlacementPastAnOpenEndSteps(t *testing.T) {
	t.Parallel()
	zs := strings.Repeat("z", 20)
	for _, c := range []struct{ name, a, b, want string }{
		{"up", "V", "", "V001"},
		{"up at a key's own length", "V001", "", "V002"},
		{"up truncates a longer key", "V00123", "", "V002"},
		{"down", "", "V", "Uzzz"},
		{"up out of a shallow corner", "zzzz", "", "zzzz0001"},
		{"down never lands on all zeros", "", "0001", "0000zzzz"},
		{"up out of this store's corner", zs, "", zs + strings.Repeat("0", 11) + "1"},
	} {
		if got, err := Midpoint(c.a, c.b); err != nil || got != c.want {
			t.Errorf("%s: Midpoint(%q, %q) = %q, %v; want %q", c.name, c.a, c.b, got, err, c.want)
		}
	}
}

func TestCeilingIsDerivedFromTheStep(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		key  string
		want int
	}{
		{"open keyspace", "V", SmoothingThreshold},
		{"past a z^20 corner", strings.Repeat("z", 20), 36},
		{"a corner no step reaches", strings.Repeat("z", 128), MaxLength},
	} {
		if got := Ceiling(c.key); got != c.want {
			t.Errorf("%s: Ceiling = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCrowdedNeedsTheCeilingAndAMovableShorterNeighbour(t *testing.T) {
	t.Parallel()
	atCeiling := "V0000001"
	for _, c := range []struct {
		name    string
		key     string
		movable []string
		want    bool
	}{
		{"under its ceiling", "V001", []string{"V"}, false},
		{"at its ceiling, longer than a neighbour", atCeiling, []string{"V0001"}, true},
		{"at its ceiling, longer than one of two", atCeiling, []string{"V0000002", "V0002"}, true},
		{"at its ceiling, no shorter neighbour", atCeiling, []string{"V0000002"}, false},
		{"at its ceiling, nothing a respace could move", atCeiling, nil, false},
	} {
		if got := Crowded(c.key, c.movable...); got != c.want {
			t.Errorf("%s: Crowded(%q, %v) = %t, want %t", c.name, c.key, c.movable, got, c.want)
		}
	}
}

func TestSpacingTakesTheShortestLengthThatFits(t *testing.T) {
	t.Parallel()
	// Two chars hold 62^2 values, so 100 keys fit there 16 apart and not in one.
	keys, err := SpacedRanksBetween("", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		if len(key) != 2 {
			t.Fatalf("key %d is %q, want 2 chars", i, key)
		}
	}
	// A respace shortens: keys spaced between far bounds forget the bounds' length.
	long := "A" + strings.Repeat("7", 60)
	keys, err = SpacedRanksBetween(long, "C", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(keys) || keys[0] <= long || keys[2] >= "C" || len(keys[0]) > 3 {
		t.Fatalf("spaced %q between a 61-char key and C, want short keys inside the bounds", keys)
	}
	if _, err := SpacedRanksBetween("A", "A000", 1); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("bounds padding to one value: %v, want ErrNoRoom", err)
	}
}

func TestPrefixTiedCountsDistinctKeysOnTheIndexPrefix(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("a", MaxLength+1)
	for _, c := range []struct {
		name string
		keys []string
		want int
	}{
		{"none", []string{"A", "B"}, 0},
		{"unranked rows are no key", []string{"", "", "A"}, 0},
		{"a duplicate is not a tie", []string{prefix + "1", prefix + "1"}, 0},
		{"at the limit keys cannot tie", []string{prefix[:MaxLength], prefix[:MaxLength] + "1", prefix[:MaxLength] + "2"}, 0},
		{"one pair past the prefix", []string{prefix + "1", prefix + "2"}, 1},
		{"a run", []string{prefix + "1", prefix + "2", prefix + "3"}, 2},
		{"a prefix of a longer key", []string{prefix, prefix + "1"}, 1},
	} {
		if got := PrefixTied(c.keys); got != c.want {
			t.Errorf("%s: PrefixTied = %d, want %d", c.name, got, c.want)
		}
	}
}
