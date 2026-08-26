package issueid

import (
	"testing"
	"time"
)

func TestGenerateHashID(t *testing.T) {
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("deterministic for identical inputs", func(t *testing.T) {
		first := GenerateHashID("test", "parser", "Fix parser", "desc", "links", createdAt, 6, 0)
		second := GenerateHashID("test", "parser", "Fix parser", "desc", "links", createdAt, 6, 0)
		if first != second {
			t.Errorf("GenerateHashID() = %q then %q, want deterministic output", first, second)
		}
	})

	t.Run("different nonce changes the ID", func(t *testing.T) {
		a := GenerateHashID("test", "parser", "Fix parser", "desc", "links", createdAt, 6, 0)
		b := GenerateHashID("test", "parser", "Fix parser", "desc", "links", createdAt, 6, 1)
		if a == b {
			t.Errorf("expected different nonces to produce different IDs, both got %q", a)
		}
	})

	t.Run("output shape is prefix-topic-hash", func(t *testing.T) {
		for _, length := range []int{MinHashLength, 5, MaxHashLength} {
			id := GenerateHashID("proj", "storage", "Title", "Description", "author", createdAt, length, 0)
			want := "proj-storage-"
			if len(id) <= len(want) || id[:len(want)] != want {
				t.Fatalf("GenerateHashID() = %q, want prefix %q-", id, want)
			}
			hashPart := id[len(want):]
			if len(hashPart) != length {
				t.Errorf("hash part %q has length %d, want %d", hashPart, len(hashPart), length)
			}
		}
	})
}

func TestHashBytesForLength(t *testing.T) {
	cases := map[int]int{
		3:  2,
		4:  3,
		5:  4,
		6:  4,
		7:  5,
		8:  5,
		99: 3, // out-of-range falls to the default
	}
	for length, want := range cases {
		if got := hashBytesForLength(length); got != want {
			t.Errorf("hashBytesForLength(%d) = %d, want %d", length, got, want)
		}
	}
}

func TestEncodeBase36(t *testing.T) {
	t.Run("all-zero hash pads to all zero characters", func(t *testing.T) {
		got := encodeBase36([]byte{0, 0, 0, 0}, 6)
		if got != "000000" {
			t.Errorf("encodeBase36(all-zero) = %q, want %q", got, "000000")
		}
	})

	t.Run("short encoding is left-padded with zeros to requested length", func(t *testing.T) {
		// A single low-value byte encodes to far fewer than 6 base36 digits.
		got := encodeBase36([]byte{1}, 6)
		if len(got) != 6 {
			t.Fatalf("encodeBase36() length = %d, want 6", len(got))
		}
		if got != "000001" {
			t.Errorf("encodeBase36([]byte{1}, 6) = %q, want %q", got, "000001")
		}
	})

	t.Run("long encoding is clamped to the requested length by taking the tail", func(t *testing.T) {
		// 4 bytes of 0xFF encode to more base36 digits than length 3 allows.
		full := encodeBase36([]byte{0xFF, 0xFF, 0xFF, 0xFF}, 20)
		clamped := encodeBase36([]byte{0xFF, 0xFF, 0xFF, 0xFF}, 3)
		if len(clamped) != 3 {
			t.Fatalf("encodeBase36() length = %d, want 3", len(clamped))
		}
		if clamped != full[len(full)-3:] {
			t.Errorf("clamped encoding %q is not the tail of the full encoding %q", clamped, full)
		}
	})
}

func TestCollisionProbability(t *testing.T) {
	t.Run("zero issues never collide", func(t *testing.T) {
		if got := CollisionProbability(0, MinHashLength); got != 0 {
			t.Errorf("CollisionProbability(0, %d) = %v, want 0", MinHashLength, got)
		}
	})

	t.Run("more issues at the same length raises collision probability", func(t *testing.T) {
		low := CollisionProbability(10, 4)
		high := CollisionProbability(10000, 4)
		if !(high > low) {
			t.Errorf("expected probability to increase with issue count: low=%v high=%v", low, high)
		}
	})

	t.Run("a longer hash lowers collision probability for the same issue count", func(t *testing.T) {
		short := CollisionProbability(1000, MinHashLength)
		long := CollisionProbability(1000, MaxHashLength)
		if !(long < short) {
			t.Errorf("expected longer hash to lower probability: short=%v long=%v", short, long)
		}
	})
}

func TestComputeAdaptiveLength(t *testing.T) {
	t.Run("stays within the configured bounds", func(t *testing.T) {
		for _, n := range []int{0, 1, 100, 1_000_000} {
			got := ComputeAdaptiveLength(n)
			if got < MinHashLength || got > MaxHashLength {
				t.Errorf("ComputeAdaptiveLength(%d) = %d, want within [%d, %d]", n, got, MinHashLength, MaxHashLength)
			}
		}
	})

	t.Run("small issue counts get the minimum length", func(t *testing.T) {
		if got := ComputeAdaptiveLength(0); got != MinHashLength {
			t.Errorf("ComputeAdaptiveLength(0) = %d, want %d", got, MinHashLength)
		}
	})

	t.Run("length never decreases as issue count grows", func(t *testing.T) {
		prev := ComputeAdaptiveLength(1)
		for _, n := range []int{10, 100, 1_000, 100_000, 10_000_000} {
			got := ComputeAdaptiveLength(n)
			if got < prev {
				t.Errorf("ComputeAdaptiveLength(%d) = %d, want >= previous %d", n, got, prev)
			}
			prev = got
		}
	})

	t.Run("an astronomically large issue count clamps to the maximum length", func(t *testing.T) {
		if got := ComputeAdaptiveLength(1 << 30); got != MaxHashLength {
			t.Errorf("ComputeAdaptiveLength(2^30) = %d, want %d", got, MaxHashLength)
		}
	})
}
