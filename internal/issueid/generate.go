package issueid

import (
	"crypto/sha256"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
)

const (
	CollisionProbabilityThreshold = 0.25
	MinHashLength                 = 3
	MaxHashLength                 = 8
	NonceAttempts                 = 10
	Base36Alphabet                = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// Namespace is the text every id in one id-space hangs under: a workspace's
// top-level space renders "<prefix>-<topic>-", one parent's child space renders
// "<parentID>.". It is the ONLY thing that differs between the two minting
// paths, so both reach Mint through the same call and there is one minting
// behavior rather than two that can drift apart.
// [LAW:one-type-per-behavior] The variability is a value crossing one boundary,
// not a second function with a second algorithm.
type Namespace string

// TopLevelNamespace is the id-space a parentless issue is minted into.
func TopLevelNamespace(prefix, topic string) Namespace {
	return Namespace(prefix + "-" + topic + "-")
}

// ChildNamespace is the id-space the direct children of parentID are minted
// into. The dot is what every reader of an id's shape keys on — the top-level
// population count excludes ids containing one — so it stays part of the
// rendering rather than becoming a second parentage record.
func ChildNamespace(parentID string) Namespace {
	return Namespace(parentID + ".")
}

// Content is the identifying material an id is derived from. CreatedAt carries
// nanosecond resolution and is what decorrelates two disconnected stores: an id
// derived from content plus the instant of creation is not a claim about what
// exists elsewhere, so two stores holding identical rows do not converge on one
// id the way a count over local rows does. It also stops a hard-deleted id
// from being handed straight to the next create the way the counter did:
// reaching it again takes a hash coincidence rather than a certainty.
//
// The guarantee is the one top-level ids have always run on, no weaker and no
// stronger, and it is probabilistic: the hash is truncated, so Mint re-rolls
// against the local store and a birthday chance remains against ids no local
// probe can see. Creator is hashed for the same reason, but the Dolt store
// stamps every create with one literal creator, so the instant carries it alone.
type Content struct {
	Topic       string
	Title       string
	Description string
	Creator     string
	CreatedAt   time.Time
}

// TakenFunc reports whether a candidate id already exists in the store. It
// carries an error because the probe is a query; a failed probe is never
// reported as "free". [LAW:no-silent-failure]
type TakenFunc func(candidate string) (bool, error)

// Mint returns an id in ns that no existing id occupies, widening the hash as
// the population grows and re-rolling on the nonce within each length.
// population is the number of ids already sharing ns, which sets the starting
// hash length; taken decides occupancy against the whole store.
//
// Those two scopes read as a mismatch and are not, which is recorded here
// because the reading recurs: a candidate rendered in ns cannot equal any id
// outside ns, so a store-wide probe can only ever report a collision with an id
// ns already holds. The spaces are disjoint by rendering — NormalizeSlug emits
// no dot, so no top-level id carries one and every child id does, and two child
// spaces differ unless their parents do, a base36 suffix carrying neither dot
// nor dash to make up the difference. Sizing the hash by ns's population
// therefore sizes it by exactly the set the probe can reach, and a three-child
// epic minting at MinHashLength is not the workspace's size leaking out of the
// estimate; it is the estimate over the only ids within reach. Top-level ids
// are the loosely sized ones: store.countTopLevelIssues counts across every
// topic though a candidate can only collide inside its own, deliberately
// conservative in the direction that costs id length rather than uniqueness.
//
// What population cannot see is the ids hard-deleted out of ns, which leave the
// count while a fresh candidate can still land on them — a freed id is
// reoccupiable by coincidence and uncountable by construction, since the delete
// takes the row. That residual is bounded and accepted rather than fixed:
// MinHashLength holds until one parent's direct children number in the
// hundreds, a boundary TestComputeAdaptiveLength pins so this claim cannot
// drift from the arithmetic.
//
// [LAW:dataflow-not-control-flow] One sweep runs for every id ever minted;
// the namespace and population are values it reads, never branches it takes.
func Mint(ns Namespace, c Content, population int, taken TakenFunc) (string, error) {
	baseLength := min(ComputeAdaptiveLength(population), MaxHashLength)
	for length := baseLength; length <= MaxHashLength; length++ {
		for nonce := 0; nonce < NonceAttempts; nonce++ {
			candidate := GenerateHashID(ns, c, length, nonce)
			occupied, err := taken(candidate)
			if err != nil {
				return "", fmt.Errorf("check issue id collision: %w", err)
			}
			if !occupied {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("generate unique issue id: exhausted lengths %d-%d", baseLength, MaxHashLength)
}

// ComputeAdaptiveLength returns the smallest hash length whose collision
// probability for the given issue count stays under CollisionProbabilityThreshold.
func ComputeAdaptiveLength(numIssues int) int {
	for length := MinHashLength; length <= MaxHashLength; length++ {
		if CollisionProbability(numIssues, length) <= CollisionProbabilityThreshold {
			return length
		}
	}
	return MaxHashLength
}

// CollisionProbability is the standard birthday-bound estimate.
func CollisionProbability(numIssues int, idLength int) float64 {
	totalPossibilities := math.Pow(36, float64(idLength))
	exponent := -float64(numIssues*numIssues) / (2.0 * totalPossibilities)
	return 1.0 - math.Exp(exponent)
}

// GenerateHashID renders one candidate id: the namespace followed by a
// base36 hash of the content plus a nonce. Same content and nonce always
// produce the same id; the nonce exists to retry on collision without
// changing the title or description.
func GenerateHashID(ns Namespace, c Content, length int, nonce int) string {
	content := fmt.Sprintf("%s|%s|%s|%s|%d|%d", c.Topic, c.Title, c.Description, c.Creator, c.CreatedAt.UnixNano(), nonce)
	hash := sha256.Sum256([]byte(content))
	return string(ns) + encodeBase36(hash[:hashBytesForLength(length)], length)
}

// hashBytesForLength is how many digest bytes an id of the given length may
// carry: enough to address every value it can render, and no more. Derived
// rather than tabulated, because a table is a second copy of this arithmetic
// and the table had already drifted from it — it handed MaxHashLength five
// bytes, 2^40 values, against 36^8 renderable ids, so the length Mint escalates
// to when a space is crowded delivered under 40% of the room CollisionProbability
// credited it with, and an out-of-range length silently fell to three bytes.
// [LAW:one-source-of-truth] sha256.Size caps the result because a digest holds
// 32 bytes and no length can spend more, not as a guard against the caller.
func hashBytesForLength(length int) int {
	renderable := new(big.Int).Exp(big.NewInt(36), big.NewInt(int64(length)), nil)
	largest := renderable.Sub(renderable, big.NewInt(1))
	return min((largest.BitLen()+7)/8, sha256.Size)
}

func encodeBase36(data []byte, length int) string {
	num := new(big.Int).SetBytes(data)
	base := big.NewInt(36)
	zero := big.NewInt(0)
	mod := new(big.Int)
	chars := make([]byte, 0, length)
	for num.Cmp(zero) > 0 {
		num.DivMod(num, base, mod)
		chars = append(chars, Base36Alphabet[mod.Int64()])
	}
	var result strings.Builder
	for i := len(chars) - 1; i >= 0; i-- {
		result.WriteByte(chars[i])
	}
	value := result.String()
	if len(value) < length {
		value = strings.Repeat("0", length-len(value)) + value
	}
	if len(value) > length {
		value = value[len(value)-length:]
	}
	return value
}
