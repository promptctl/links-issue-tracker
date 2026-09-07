package issueid

import (
	"crypto/sha256"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestGenerateHashID(t *testing.T) {
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	content := Content{Topic: "parser", Title: "Fix parser", Description: "desc", Creator: "links", CreatedAt: createdAt}
	topLevel := TopLevelNamespace("test", "parser")

	t.Run("deterministic for identical inputs", func(t *testing.T) {
		first := GenerateHashID(topLevel, content, 6, 0)
		second := GenerateHashID(topLevel, content, 6, 0)
		if first != second {
			t.Errorf("GenerateHashID() = %q then %q, want deterministic output", first, second)
		}
	})

	t.Run("different nonce changes the ID", func(t *testing.T) {
		a := GenerateHashID(topLevel, content, 6, 0)
		b := GenerateHashID(topLevel, content, 6, 1)
		if a == b {
			t.Errorf("expected different nonces to produce different IDs, both got %q", a)
		}
	})

	// Every field of Content reaches the hash — the property Content's own doc
	// comment claims. Asserted field by field, in the order GenerateHashID
	// renders them, so dropping one from that format string fails here instead
	// of silently collapsing two siblings born in one nanosecond onto one id.
	// [LAW:one-type-per-behavior] The fields are rows, not five subtests.
	for _, field := range []struct {
		name  string
		alter func(*Content)
	}{
		{"topic", func(c *Content) { c.Topic = "renderer" }},
		{"title", func(c *Content) { c.Title = "Fix the other parser" }},
		{"description", func(c *Content) { c.Description = "other desc" }},
		{"creator", func(c *Content) { c.Creator = "someone-else" }},
		{"creation instant", func(c *Content) { c.CreatedAt = createdAt.Add(time.Nanosecond) }},
	} {
		t.Run("a different "+field.name+" changes the ID", func(t *testing.T) {
			other := content
			field.alter(&other)
			base := GenerateHashID(topLevel, content, 6, 0)
			if base == GenerateHashID(topLevel, other, 6, 0) {
				t.Errorf("a different %s still minted %q; every Content field must reach the hash", field.name, base)
			}
		})
	}

	t.Run("a top-level id renders under prefix-topic-", func(t *testing.T) {
		assertNamespacedShape(t, TopLevelNamespace("proj", "storage"), "proj-storage-", content)
	})

	t.Run("a child id renders under its parent id and a dot", func(t *testing.T) {
		assertNamespacedShape(t, ChildNamespace("proj-storage-a7k9"), "proj-storage-a7k9.", content)
	})
}

// assertNamespacedShape checks that every hash length renders as the namespace
// followed by exactly that many hash characters — the property both id-spaces
// share, asserted once rather than per namespace.
func assertNamespacedShape(t *testing.T, ns Namespace, want string, content Content) {
	t.Helper()
	for _, length := range []int{MinHashLength, 5, MaxHashLength} {
		id := GenerateHashID(ns, content, length, 0)
		suffix, ok := strings.CutPrefix(id, want)
		if !ok {
			t.Fatalf("GenerateHashID() = %q, want prefix %q", id, want)
		}
		if len(suffix) != length {
			t.Errorf("hash part %q has length %d, want %d", suffix, len(suffix), length)
		}
	}
}

func TestMint(t *testing.T) {
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	content := Content{Topic: "sync", Title: "A child", Description: "d", Creator: "links", CreatedAt: createdAt}

	t.Run("re-rolls past an occupied id", func(t *testing.T) {
		first := GenerateHashID(ChildNamespace("p"), content, MinHashLength, 0)
		got, err := Mint(ChildNamespace("p"), content, 0, func(candidate string) (bool, error) {
			return candidate == first, nil
		})
		if err != nil {
			t.Fatalf("Mint() error = %v", err)
		}
		if got == first {
			t.Errorf("Mint() = %q, want an id other than the occupied %q", got, first)
		}
	})

	t.Run("a failed occupancy probe is an error, never a free id", func(t *testing.T) {
		_, err := Mint(ChildNamespace("p"), content, 0, func(string) (bool, error) {
			return false, errors.New("store unreachable")
		})
		if err == nil {
			t.Fatal("Mint() error = nil, want the probe failure surfaced")
		}
		if !strings.Contains(err.Error(), "store unreachable") {
			t.Errorf("Mint() error = %v, want it to carry the probe failure", err)
		}
	})

	t.Run("reports exhaustion instead of returning a taken id", func(t *testing.T) {
		_, err := Mint(ChildNamespace("p"), content, 0, func(string) (bool, error) {
			return true, nil
		})
		if err == nil {
			t.Fatal("Mint() error = nil, want exhaustion reported")
		}
	})
}

// TestNamespacesAreDisjoint pins the property Mint's length policy rests on: an
// id rendered in one id-space can never equal an id rendered in another, so the
// namespace population Mint sizes the hash by IS the whole set the store-wide
// occupancy probe can reach. The two legs are asserted where they live — a slug
// carries no dot (TestNormalizeSlugEmitsOnlyItsAlphabet), and a rendered suffix
// carries neither dot nor dash, here. If this fails, the population feeding
// ComputeAdaptiveLength has stopped being the colliding set and the policy
// recorded on Mint needs revisiting rather than this test relaxing.
// [LAW:behavior-not-structure]
func TestNamespacesAreDisjoint(t *testing.T) {
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	if Base36Alphabet != base36 {
		t.Fatalf("Base36Alphabet = %q, want %q; the disjointness below is stated over this alphabet", Base36Alphabet, base36)
	}

	// The id-spaces one workspace mints into, including the pair that is kept
	// apart by the separator alone: a topic slug may carry dashes, so the topic
	// "storage-a7k9" reaches the same characters as a child space of the parent
	// "proj-storage-a7k9" would if a child hung under a dash.
	spaces := map[string]Namespace{
		"top-level":                    TopLevelNamespace("proj", "storage"),
		"another topic":                TopLevelNamespace("proj", "sync"),
		"a topic spelling a parent id": TopLevelNamespace("proj", "storage-a7k9"),
		"a child space":                ChildNamespace("proj-storage-a7k9"),
		"a sibling's space":            ChildNamespace("proj-storage-b1x2"),
		"a grandchild space":           ChildNamespace("proj-storage-a7k9.m3p"),
	}

	t.Run("no two id-spaces render as one namespace", func(t *testing.T) {
		byText := map[Namespace]string{}
		for name, ns := range spaces {
			if other, clash := byText[ns]; clash {
				t.Errorf("%q and %q both render %q; two id-spaces under one namespace share a population the other never counted", other, name, ns)
			}
			byText[ns] = name
		}
	})

	t.Run("where one namespace prefixes another, the remainder leaves base36", func(t *testing.T) {
		// An id is its namespace followed by base36 and nothing else, so ids of A
		// can reach into B's space exactly when B extends A by base36 alone.
		for aName, a := range spaces {
			for bName, b := range spaces {
				remainder, extends := strings.CutPrefix(string(b), string(a))
				if a == b || !extends {
					continue
				}
				if strings.Trim(remainder, base36) == "" {
					t.Errorf("%q extends %q by %q, which is all base36: an id of %q can render as one of %q", bName, aName, remainder, aName, bName)
				}
			}
		}
	})

	t.Run("a rendered suffix is base36 only, so it cannot carry a separator", func(t *testing.T) {
		content := Content{Topic: "sync", Title: "t", Description: "d", Creator: "links", CreatedAt: time.Unix(0, 1)}
		for name, ns := range spaces {
			for length := MinHashLength; length <= MaxHashLength; length++ {
				suffix, ok := strings.CutPrefix(GenerateHashID(ns, content, length, 0), string(ns))
				if !ok {
					t.Fatalf("%s: id does not render under its own namespace %q", name, ns)
				}
				if stray := strings.Trim(suffix, base36); stray != "" {
					t.Errorf("%s at length %d: suffix %q carries %q from outside base36", name, length, suffix, stray)
				}
			}
		}
	})
}

// TestHashBytesForLength states the contract as a property rather than a table
// of answers: the byte count is the fewest whose value space addresses every id
// the length can render. The expectation is formulated independently of the
// implementation — a doubling loop rather than the same bits-to-bytes rounding
// — because a table restating the arithmetic is exactly what drifted from it.
// [LAW:behavior-not-structure]
func TestHashBytesForLength(t *testing.T) {
	byteSpace := func(bytes int) *big.Int { return new(big.Int).Lsh(big.NewInt(1), uint(8*bytes)) }

	for length := MinHashLength; length <= MaxHashLength; length++ {
		renderable := new(big.Int).Exp(big.NewInt(36), big.NewInt(int64(length)), nil)
		got := hashBytesForLength(length)
		if byteSpace(got).Cmp(renderable) < 0 {
			t.Errorf("hashBytesForLength(%d) = %d bytes, too few to address all 36^%d ids", length, got, length)
		}
		if byteSpace(got-1).Cmp(renderable) >= 0 {
			t.Errorf("hashBytesForLength(%d) = %d bytes, but %d already covers 36^%d", length, got, got-1, length)
		}
	}

	t.Run("MaxHashLength gets the six bytes its space needs", func(t *testing.T) {
		// The entry the old table got wrong, called out by name because this is
		// the length Mint escalates to when a namespace is crowded: five bytes
		// address 2^40 values against 36^8, under half of them.
		if got := hashBytesForLength(MaxHashLength); got != 6 {
			t.Errorf("hashBytesForLength(MaxHashLength) = %d, want 6", got)
		}
	})

	t.Run("a length wider than a digest stops at the digest", func(t *testing.T) {
		if got := hashBytesForLength(99); got != sha256.Size {
			t.Errorf("hashBytesForLength(99) = %d, want %d, all a digest holds", got, sha256.Size)
		}
	})
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

	// Mint's recorded length policy leans on where the floor gives way: a
	// namespace is one parent's direct children, and 163 is far past any epic,
	// which is what makes the floor the right size for a child rather than a
	// shortfall. Pinned so that claim cannot drift from the arithmetic.
	t.Run("the minimum length holds to 163 ids in one namespace", func(t *testing.T) {
		if got := ComputeAdaptiveLength(163); got != MinHashLength {
			t.Errorf("ComputeAdaptiveLength(163) = %d, want %d", got, MinHashLength)
		}
		if got := ComputeAdaptiveLength(164); got <= MinHashLength {
			t.Errorf("ComputeAdaptiveLength(164) = %d, want more than %d", got, MinHashLength)
		}
	})

	t.Run("an astronomically large issue count clamps to the maximum length", func(t *testing.T) {
		if got := ComputeAdaptiveLength(1 << 30); got != MaxHashLength {
			t.Errorf("ComputeAdaptiveLength(2^30) = %d, want %d", got, MaxHashLength)
		}
	})
}
