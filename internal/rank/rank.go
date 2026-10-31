// Package rank implements lexicographic fractional indexing for work item ordering.
//
// Ranks are strings from a base-62 alphabet (0-9A-Za-z) that support efficient
// SQL ORDER BY. A key is at most MaxLength chars, so distinct keys never tie on
// the index that streams them. Where a spot has no room left at that length,
// Respace makes some.
package rank

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// alphabet is the ordered character set. Every character's sort position
// is its index in this string, so string comparison matches rank ordering.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const base = len(alphabet) // 62

// charIndex maps each alphabet byte to its ordinal position.
var charIndex [256]int

var (
	bigBase = big.NewInt(int64(base))
	bigOne  = big.NewInt(1)
)

func init() {
	for i := range charIndex {
		charIndex[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		charIndex[alphabet[i]] = i
	}
}

// Valid reports whether s is a well-formed, stored rank value: a non-empty
// string whose every byte is a member of the ordering alphabet. The empty
// string is not a stored rank — it denotes "unranked" — so it is not Valid.
//
// This predicate is for validating PERSISTED rank values (e.g. checking a
// rebuilt workspace). It is NOT the input contract of the ordering primitives:
// Midpoint and SpacedRanksBetween deliberately accept an empty bound as a
// sentinel ("before/after everything"), so callers must not pre-screen those
// bounds with Valid. Callers checking a stored rank consult this one definition rather than
// re-deriving the alphabet.
func Valid(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if charIndex[s[i]] < 0 {
			return false
		}
	}
	return true
}

// Significant returns s without its trailing zeros: the part that decides where
// s sorts once padded. Two ranks with the same significant part leave
// SpacedRanksBetween and Midpoint no room between them.
func Significant(s string) string {
	return strings.TrimRight(s, "0")
}

// Anchor returns what s bounds a gap by: Significant, of what the index holds.
// Two keys with the same Anchor leave no room or read in the wrong order, so a
// repair keeps at most one of them where it stands.
func Anchor(s string) string {
	return Significant(indexPrefix(s))
}

// ErrNoRoom reports bounds with nothing to spend between them. A pair with the
// same significant part, one the other extended by zeros or an empty lower bound
// meeting an all-zero upper one, holds only further zero-extensions of the lower
// bound, each of which leaves no room beside itself, so the primitives refuse it
// instead of spending that sliver. Any other pair is refused when its keys would
// pass MaxLength. A caller holding one reads it as "respace these ranks first",
// never as a bad request.
var ErrNoRoom = errors.New("rank: no room between the bounds")

// Midpoint returns a key that sorts strictly between a and b, at the shortest
// length with room: a gap of 4 or more at length L yields a key of at most L
// chars, which is what lets a respaced window take the next placement.
// Precondition: a < b (lexicographic). Returns an error if a >= b, and
// ErrNoRoom if the bounds share a significant part or leave no room inside
// MaxLength. An empty a means "before everything" and an empty b "after
// everything", and past an open end a key steps rather than bisects, so appends
// and prepends never deepen a corner. Both empty is the whole keyspace, whose
// midpoint is the first key a workspace holds.
func Midpoint(a, b string) (string, error) {
	if a == b && a != "" {
		return "", errors.New("rank: a and b are equal")
	}
	if a != "" && b != "" && a >= b {
		return "", errors.New("rank: a must be less than b")
	}
	if a != "" && b == "" {
		return step(a, 1)
	}
	if a == "" && b != "" {
		return step(b, -1)
	}
	keys, err := spacedRanks(1, a, b, 1)
	if err != nil {
		return "", err
	}
	return keys[0], nil
}

// MaxLength is the longest key the package writes: one short of
// idx_issues_rank's 191-char prefix. Dolt streams ORDER BY item_rank from that
// index without sorting, so two keys tied on the prefix read in the wrong
// order with no error. A key shorter than the prefix cannot tie with any other.
const MaxLength = 190

// SmoothingThreshold is the ceiling of a key in open keyspace, the length past
// which its spot may be crowded. Ceiling derives every other spot's from it.
const SmoothingThreshold = 8

// SmoothingWindow is the width a respace starts from, half of it a side.
const SmoothingWindow = 32

// spacingGap is the units a spaced key keeps from its neighbours.
const spacingGap = 16

// SpacedRanks returns n evenly-spaced rank strings that span the full keyspace.
func SpacedRanks(n int) []string {
	ranks, err := spacedRanks(n, "", "", spacingGap)
	if err != nil {
		// [LAW:one-source-of-truth] empty bounds are the canonical full keyspace; this path must stay valid.
		panic(fmt.Sprintf("rank: spaced ranks with empty bounds failed: %v", err))
	}
	return ranks
}

// SpacedRanksBetween returns n evenly-spaced rank strings between lower and
// upper bounds (exclusive). Empty lower means "before everything", empty upper
// means "after everything".
//
// It fails on a negative n, on a non-empty lower at or above a non-empty upper,
// and on bounds with no room for the ranks asked for: bounds that pad to the
// same value — one being the other extended by zeros — leave no room for a rank
// longer than both, and a caller reading its bounds out of a store can be
// handed such a pair.
func SpacedRanksBetween(lower, upper string, n int) ([]string, error) {
	if lower != "" && upper != "" && lower >= upper {
		return nil, errors.New("rank: lower must be less than upper")
	}
	return spacedRanks(n, lower, upper, spacingGap)
}

// spacedRanks places n keys between the bounds at the shortest length that keeps
// minGap units around each, so a respace shortens the keys it touches.
func spacedRanks(n int, lower, upper string, minGap int64) ([]string, error) {
	// [LAW:single-enforcer] Size validation lives at the shared spacing boundary so all callers get identical behavior.
	if n < 0 {
		return nil, errors.New("rank: n must be non-negative")
	}
	if n == 0 {
		return nil, nil
	}
	if upper != "" && Significant(lower) == Significant(upper) {
		return nil, fmt.Errorf("%w: %q and %q pad to the same value, so no rank sorts between them", ErrNoRoom, lower, upper)
	}
	denominator := big.NewInt(int64(n + 1))
	minGapBig := big.NewInt(minGap)
	// No key fits inside the prefix the bounds share.
	for length := sharedPrefix(lower, upper) + 1; length <= MaxLength; length++ {
		lo, err := lowerBoundInt(lower, length)
		if err != nil {
			return nil, err
		}
		hi, err := upperBoundInt(upper, length)
		if err != nil {
			return nil, err
		}
		span := new(big.Int).Sub(hi, lo)
		gap := new(big.Int).Div(span, denominator)
		if gap.Cmp(minGapBig) < 0 {
			continue
		}
		out := make([]string, n)
		for i := range out {
			offset := new(big.Int).Mul(gap, big.NewInt(int64(i+1)))
			value := new(big.Int).Add(lo, offset)
			encoded, err := encodeBase62(value, length)
			if err != nil {
				return nil, err
			}
			out[i] = encoded
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: %d ranks between %q and %q would pass %d chars", ErrNoRoom, n, lower, upper, MaxLength)
}

func sharedPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// lowerBoundInt returns the first integer value at the given string length
// that is strictly greater than s. Empty s means the absolute minimum (0).
func lowerBoundInt(s string, length int) (*big.Int, error) {
	if s == "" {
		return big.NewInt(0), nil
	}
	v, err := stringToInt(s, length)
	if err != nil {
		return nil, err
	}
	if len(s) >= length {
		return new(big.Int).Add(v, bigOne), nil // need strictly greater
	}
	return v, nil // padding already makes it > s
}

// upperBoundInt returns the last integer value at the given string length
// that is strictly less than s. Empty s means the absolute maximum.
func upperBoundInt(s string, length int) (*big.Int, error) {
	if s == "" {
		return pow62(length), nil
	}
	// stringToInt pads with '0' (index 0). When len(s) < length, the padded
	// value is the first length-char string starting with s, which is > s.
	// When len(s) == length, the padded value equals s exactly.
	// In both cases, we want strictly less than s, so subtract 1.
	v, err := stringToInt(s, length)
	if err != nil {
		return nil, err
	}
	return new(big.Int).Sub(v, bigOne), nil
}

// stringToInt converts a rank string to an integer, padding with '0' (index 0)
// on the right to reach the target length.
func stringToInt(s string, length int) (*big.Int, error) {
	v := big.NewInt(0)
	for i := 0; i < length; i++ {
		v.Mul(v, bigBase)
		if i < len(s) {
			idx := charIndex[s[i]]
			if idx < 0 {
				return nil, errors.New("rank: invalid character in bounds")
			}
			v.Add(v, big.NewInt(int64(idx)))
		}
		// else: pad with 0 (index of '0' in alphabet)
	}
	return v, nil
}

func pow62(n int) *big.Int {
	v := big.NewInt(1)
	for i := 0; i < n; i++ {
		v.Mul(v, bigBase)
	}
	return v
}

// encodeBase62 encodes value as a fixed-width base-62 string of the given length.
func encodeBase62(value *big.Int, length int) (string, error) {
	if value.Sign() < 0 {
		return "", errors.New("rank: cannot encode negative value")
	}
	buf := make([]byte, length)
	quotient := new(big.Int).Set(value)
	remainder := new(big.Int)
	for i := length - 1; i >= 0; i-- {
		quotient.QuoRem(quotient, bigBase, remainder)
		idx := int(remainder.Int64())
		if idx < 0 || idx >= base {
			return "", errors.New("rank: base62 remainder out of range")
		}
		buf[i] = alphabet[idx]
	}
	if quotient.Sign() != 0 {
		return "", errors.New("rank: value does not fit fixed-width encoding")
	}
	return string(buf), nil
}

// stepLength is the shortest length a key past an open end is stepped at.
const stepLength = 4

// step moves s one unit in direction dir, at the shortest power-of-two length
// from stepLength up at which such a key exists.
func step(s string, dir int64) (string, error) {
	if dir < 0 && Significant(s) == "" {
		return "", fmt.Errorf("%w: nothing sorts below %q but the empty string", ErrNoRoom, s)
	}
	for length := stepLength; length <= MaxLength; length *= 2 {
		v, err := stringToInt(s, length)
		if err != nil {
			return "", err
		}
		v.Add(v, big.NewInt(dir))
		if v.Sign() > 0 && v.Cmp(pow62(length)) < 0 {
			return encodeBase62(v, length)
		}
	}
	return "", fmt.Errorf("%w: no step from %q fits in %d chars", ErrNoRoom, s, MaxLength)
}

// Ceiling is how long a key at key's place may grow before smoothing: the
// length a step from it takes, plus the headroom SmoothingThreshold allows a
// stepLength key. A place no step reaches has only MaxLength above it.
func Ceiling(key string) int {
	return min(max(stepWidth(key, 1), stepWidth(key, -1))+SmoothingThreshold-stepLength, MaxLength)
}

func stepWidth(key string, dir int64) int {
	stepped, err := step(key, dir)
	if err != nil {
		return MaxLength
	}
	return len(stepped)
}

// Crowded reports whether key means its spot's gaps are spent: it reached its
// ceiling and bisection went deeper than a neighbour a respace could move. An
// edge key is not one of those: it never moves, so its length says nothing
// about density here, and a spot with no such neighbour has nothing to respace.
func Crowded(key string, movable ...string) bool {
	return len(key) >= Ceiling(key) && slices.ContainsFunc(movable, func(n string) bool { return len(key) > len(n) })
}

// MaxRespace is the most rows a window widens to for length alone.
const MaxRespace = 256

// Window is the rows a respace would move at one width: Rows of them between
// the keys Lower and Upper, each side's bound staying outside. An empty bound
// is an open end. Whole is every ranked row when neither side has a key left to
// widen to, and 0 until then.
type Window struct {
	Lower, Upper string
	Rows, Whole  int
}

// leavesStep reports whether keys leave a placement past each end of them. A
// bound that looks closed can be the row a move is vacating, which opens that
// end for the next placement, so a window in a corner too deep to step out of
// is spaced and still refuses it: no room at all.
func leavesStep(keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	_, past := step(keys[len(keys)-1], 1)
	_, before := step(keys[0], -1)
	return past == nil && before == nil
}

// Respace returns the keys the window around trigger takes, in order. The
// window starts at SmoothingWindow/2 keys a side and doubles while its keys come
// out at or past the trigger's ceiling, up to MaxRespace, where key length
// absorbs the density, and past that only while its bounds leave no room. A
// window with no room between the store's two edges is every row: only then are
// both sides open, and whole says the keys are for all of them. read returns
// the window of k keys a side: the store's is SQL, and the loop never sees where
// the rows live. [LAW:effects-at-boundaries]
func Respace(trigger string, read func(k int) (Window, error)) (keys []string, whole bool, err error) {
	for k := SmoothingWindow / 2; ; k *= 2 {
		window, err := read(k)
		if err != nil {
			return nil, false, err
		}
		keys, err := SpacedRanksBetween(window.Lower, window.Upper, window.Rows)
		if err == nil && !leavesStep(keys) {
			err = fmt.Errorf("%w: no step fits past %d spaced keys", ErrNoRoom, len(keys))
		}
		reachedEdges := window.Whole > 0
		if errors.Is(err, ErrNoRoom) && reachedEdges {
			keys, err = SpacedRanksBetween("", "", window.Whole)
			return keys, true, err
		}
		switch {
		case errors.Is(err, ErrNoRoom):
		case err != nil:
			return nil, false, err
		case len(keys) == 0, len(keys[0]) < Ceiling(trigger), 2*k >= MaxRespace, reachedEdges:
			return keys, false, nil
		}
	}
}

// PrefixTied counts the adjacent keys in sorted that are distinct and equal on
// their first MaxLength+1 chars, the prefix idx_issues_rank holds. A key this
// package wrote cannot be one. Dolt reads such a pair in the wrong order with no
// error, so a store that inherited one has to be told. An unranked row is no
// key, and an equal pair is not a tie: a duplicate key is a state of its own.
func PrefixTied(sorted []string) int {
	tied := 0
	for i := 1; i < len(sorted); i++ {
		a, b := sorted[i-1], sorted[i]
		if a != "" && a != b && indexPrefix(a) == indexPrefix(b) {
			tied++
		}
	}
	return tied
}

func indexPrefix(key string) string {
	return key[:min(len(key), MaxLength+1)]
}
