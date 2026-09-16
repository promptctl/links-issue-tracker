package rank

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInitial(t *testing.T) {
	r := Initial()
	if r == "" {
		t.Fatal("Initial() returned empty string")
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},       // empty is "unranked", not a stored rank
		{Initial(), true}, // the package's own starting rank
		{"V", true},       // single alphabet char
		{"0z", true},      // multi-byte, alphabet bounds
		{"aZ09", true},    // mixed multi-byte
		{"-", false},      // outside the alphabet
		{"V!", false},     // one bad byte among good
		{" ", false},      // space is not in the alphabet
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

func TestBefore(t *testing.T) {
	initial := Initial()
	b, err := Before(initial)
	if err != nil {
		t.Fatalf("Before(%q) error: %v", initial, err)
	}
	if b >= initial {
		t.Errorf("Before(%q) = %q, want result < %q", initial, b, initial)
	}
}

func TestAfter(t *testing.T) {
	initial := Initial()
	a := After(initial)
	if a <= initial {
		t.Errorf("After(%q) = %q, want result > %q", initial, a, initial)
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
// bounds. Midpoint("10", "100") once returned "100V", which sorts above both.
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
// rank, so Before has nothing to return for one.
func TestBeforeAnAllZeroRankHasNoRoom(t *testing.T) {
	for _, a := range []string{"0", "000"} {
		if got, err := Before(a); !errors.Is(err, ErrNoRoom) {
			t.Errorf("Before(%q) = %q, %v; want ErrNoRoom", a, got, err)
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

func TestSequentialAfter(t *testing.T) {
	// Create 1000 items by repeatedly calling After — verify strictly increasing.
	ranks := make([]string, 1001)
	ranks[0] = Initial()
	for i := 1; i <= 1000; i++ {
		ranks[i] = After(ranks[i-1])
		if ranks[i] <= ranks[i-1] {
			t.Fatalf("After(%q) = %q at step %d, not strictly increasing", ranks[i-1], ranks[i], i)
		}
	}
}

func TestSequentialBefore(t *testing.T) {
	// Create 1000 items by repeatedly calling Before — verify strictly decreasing.
	ranks := make([]string, 1001)
	ranks[0] = Initial()
	for i := 1; i <= 1000; i++ {
		r, err := Before(ranks[i-1])
		if err != nil {
			t.Fatalf("Before(%q) at step %d error: %v", ranks[i-1], i, err)
		}
		ranks[i] = r
		if ranks[i] >= ranks[i-1] {
			t.Fatalf("Before(%q) = %q at step %d, not strictly decreasing", ranks[i-1], ranks[i], i)
		}
	}
}

func TestRepeatedMidpointInsertion(t *testing.T) {
	// Repeatedly insert between the same pair — the stress scenario for string growth.
	lo := "A"
	hi := "B"
	for i := 0; i < 10000; i++ {
		mid, err := Midpoint(lo, hi)
		if err != nil {
			t.Fatalf("step %d: Midpoint(%q, %q) error: %v", i, lo, hi, err)
		}
		if mid <= lo || mid >= hi {
			t.Fatalf("step %d: Midpoint(%q, %q) = %q, ordering violated", i, lo, hi, mid)
		}
		// Always insert at the bottom of the gap to maximize string growth (worst case).
		hi = mid
	}
	// After 10k insertions, verify string length is bounded.
	// Worst case: each character position gives log2(62) ≈ 6 bisections,
	// so 10k insertions → ~1700 chars. This is the pathological case of
	// always inserting at the same end of a shrinking gap.
	if len(hi) > 2500 {
		t.Errorf("after 10k insertions, rank length is %d, expected < 2500", len(hi))
	}
	t.Logf("after 10k same-spot insertions: rank length = %d", len(hi))
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
	ranks := []string{Initial()}
	for i := 0; i < 5; i++ {
		ranks = append(ranks, After(ranks[len(ranks)-1]))
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

// A frame with nothing ranked has two open ends, and the store seeds it with
// the midpoint of that pair, so that midpoint must be the one Initial names.
func TestMidpointOfTheWholeKeyspaceIsInitial(t *testing.T) {
	t.Parallel()
	if got, err := Midpoint("", ""); err != nil || got != Initial() {
		t.Fatalf("Midpoint(\"\", \"\") = %q, %v; want %q", got, err, Initial())
	}
}

// The empty string is an unranked row to Before and After, not the open end it
// is to Midpoint, so neither answers it with the whole keyspace's midpoint.
func TestBeforeAndAfterRefuseTheEmptyRank(t *testing.T) {
	t.Parallel()
	if got, err := Before(""); err == nil {
		t.Fatalf("Before(\"\") = %q, nil; want an error", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("After(\"\") did not panic")
		}
	}()
	_ = After("")
}
