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

// GenerateHashID builds a deterministic ID from the issue's identifying
// fields plus a nonce. Same inputs + same nonce always produce the same ID;
// the nonce exists to retry on collision without changing the title or
// description.
func GenerateHashID(prefix string, topic string, title string, description string, creator string, createdAt time.Time, length int, nonce int) string {
	content := fmt.Sprintf("%s|%s|%s|%s|%d|%d", topic, title, description, creator, createdAt.UnixNano(), nonce)
	hash := sha256.Sum256([]byte(content))
	shortHash := encodeBase36(hash[:hashBytesForLength(length)], length)
	return fmt.Sprintf("%s-%s-%s", prefix, topic, shortHash)
}

func hashBytesForLength(length int) int {
	switch length {
	case 3:
		return 2
	case 4:
		return 3
	case 5, 6:
		return 4
	case 7, 8:
		return 5
	default:
		return 3
	}
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
