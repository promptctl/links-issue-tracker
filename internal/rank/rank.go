// Package rank implements lexicographic fractional indexing for work item ordering.
//
// Ranks are strings from a base-62 alphabet (0-9A-Za-z) that support
// efficient SQL ORDER BY and guarantee midpoint insertion between any
// two distinct values without rebalancing.
package rank

import (
	"errors"
	"strings"
)

// alphabet is the ordered character set. Every character's sort position
// is its index in this string, so string comparison matches rank ordering.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const base = len(alphabet) // 62

// charIndex maps each alphabet byte to its ordinal position.
var charIndex [256]int

func init() {
	for i := range charIndex {
		charIndex[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		charIndex[alphabet[i]] = i
	}
}

// Initial returns a starting rank for the first item in a fresh workspace.
func Initial() string {
	return string(alphabet[base/2]) // "V"
}

// Midpoint returns a string that sorts strictly between a and b.
// Precondition: a < b (lexicographic). Returns an error if a >= b.
// Either a or b (but not both) may be empty: empty-a means "before everything",
// empty-b means "after everything".
func Midpoint(a, b string) (string, error) {
	if a == b {
		return "", errors.New("rank: a and b are equal")
	}
	if a != "" && b != "" && a >= b {
		return "", errors.New("rank: a must be less than b")
	}
	// Walk character positions, building the result.
	var out strings.Builder
	i := 0
	for {
		aChar := 0 // virtual character below the alphabet floor
		if i < len(a) {
			aChar = charIndex[a[i]]
			if aChar < 0 {
				return "", errors.New("rank: invalid character in a")
			}
		}
		bChar := base // virtual character above the alphabet ceiling
		if i < len(b) {
			bChar = charIndex[b[i]]
			if bChar < 0 {
				return "", errors.New("rank: invalid character in b")
			}
		}

		// If there is room between aChar and bChar at this position, pick the midpoint.
		if bChar-aChar > 1 {
			mid := aChar + (bChar-aChar)/2
			out.WriteByte(alphabet[mid])
			return out.String(), nil
		}

		// Characters are adjacent or equal at this position.
		// Emit the lower character and carry to the next position.
		// When aChar == bChar we continue scanning deeper.
		// When bChar == aChar+1 we emit aChar and narrow against b's deeper digits.
		out.WriteByte(alphabet[aChar])
		i++

		// After emitting aChar, the effective lower bound for the next position is:
		// - if i < len(a): a[i] (we must exceed the remaining suffix of a)
		// - if i >= len(a): 0 (any character will do)
		// The effective upper bound is:
		// - if aChar == bChar and i < len(b): b[i] (we must stay below b's suffix)
		// - if bChar == aChar+1: base (any character works, since we already emitted a
		//   character smaller than b's character at this position)
		// This is handled by the next loop iteration naturally.
	}
}

// SmoothingThreshold is the rank string length that triggers local smoothing.
// Normal ranks are 1-6 chars; anything beyond this means the local region
// is getting dense and should be re-spaced.
const SmoothingThreshold = 8

// SmoothingWindow is the number of items to re-space during local smoothing.
const SmoothingWindow = 32

// SpacedRanks returns n evenly-spaced rank strings that span the full keyspace.
func SpacedRanks(n int) []string {
	return spacedRanks(n, "", "")
}

// SpacedRanksBetween returns n evenly-spaced rank strings between lower and
// upper bounds (exclusive). Empty lower means "before everything", empty upper
// means "after everything".
func SpacedRanksBetween(lower, upper string, n int) ([]string, error) {
	if n == 0 {
		return nil, nil
	}
	if lower != "" && upper != "" && lower >= upper {
		return nil, errors.New("rank: lower must be less than upper")
	}
	return spacedRanks(n, lower, upper), nil
}

func spacedRanks(n int, lower, upper string) []string {
	if n == 0 {
		return nil
	}
	// Find the minimum string length that fits n items with comfortable spacing.
	const minGap = 16
	maxLen := len(lower)
	if len(upper) > maxLen {
		maxLen = len(upper)
	}
	for length := maxLen + 1; ; length++ {
		lo := lowerBoundInt(lower, length)
		hi := upperBoundInt(upper, length)
		span := hi - lo
		if span <= 0 {
			continue
		}
		if span/(n+1) >= minGap {
			step := span / (n + 1)
			out := make([]string, n)
			for i := range out {
				out[i] = encodeBase62(lo+step*(i+1), length)
			}
			return out
		}
	}
}

// lowerBoundInt returns the first integer value at the given string length
// that is strictly greater than s. Empty s means the absolute minimum (0).
func lowerBoundInt(s string, length int) int {
	if s == "" {
		return 0
	}
	v := stringToInt(s, length)
	if len(s) >= length {
		return v + 1 // need strictly greater
	}
	return v // padding already makes it > s
}

// upperBoundInt returns the last integer value at the given string length
// that is strictly less than s. Empty s means the absolute maximum.
func upperBoundInt(s string, length int) int {
	if s == "" {
		return pow62(length)
	}
	// stringToInt pads with '0' (index 0). When len(s) < length, the padded
	// value is the first length-char string starting with s, which is > s.
	// When len(s) == length, the padded value equals s exactly.
	// In both cases, we want strictly less than s, so subtract 1.
	return stringToInt(s, length) - 1
}

// stringToInt converts a rank string to an integer, padding with '0' (index 0)
// on the right to reach the target length.
func stringToInt(s string, length int) int {
	v := 0
	for i := 0; i < length; i++ {
		v *= base
		if i < len(s) {
			v += charIndex[s[i]]
		}
		// else: pad with 0 (index of '0' in alphabet)
	}
	return v
}

func pow62(n int) int {
	v := 1
	for i := 0; i < n; i++ {
		v *= base
	}
	return v
}

// encodeBase62 encodes value as a fixed-width base-62 string of the given length.
func encodeBase62(value int, length int) string {
	buf := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		buf[i] = alphabet[value%base]
		value /= base
	}
	return string(buf)
}

// Before returns a rank that sorts before the given rank.
// Equivalent to Midpoint("", a).
func Before(a string) string {
	r, err := Midpoint("", a)
	if err != nil {
		// Only possible if a is empty, which callers should not do.
		panic("rank.Before called with empty string")
	}
	return r
}

// After returns a rank that sorts after the given rank.
// Equivalent to Midpoint(a, "").
func After(a string) string {
	r, err := Midpoint(a, "")
	if err != nil {
		// Only possible if a is empty, which callers should not do.
		panic("rank.After called with empty string")
	}
	return r
}
