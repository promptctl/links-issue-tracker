package rank

import (
	"testing"
)

func TestInitial(t *testing.T) {
	r := Initial()
	if r == "" {
		t.Fatal("Initial() returned empty string")
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
	b := Before(initial)
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
		ranks[i] = Before(ranks[i-1])
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
