package issueid

import (
	"fmt"
	"strings"
)

const (
	PrefixMinLength = 3
	PrefixMaxLength = 12
	TopicMinLength  = 3
	TopicMaxLength  = 30
)

func NormalizeSlug(input string) string {
	var builder strings.Builder
	previousDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(input)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			previousDash = false
		case !previousDash:
			builder.WriteByte('-')
			previousDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func NormalizeConfiguredPrefix(input string) (string, error) {
	// [LAW:single-enforcer] Prefix length and slug rules are enforced once in the shared ID boundary so config and storage cannot drift.
	normalized := NormalizeSlug(input)
	if normalized == "" {
		return "", fmt.Errorf("issue prefix is required")
	}
	if len(normalized) > PrefixMaxLength {
		normalized = normalized[:PrefixMaxLength]
		normalized = strings.Trim(normalized, "-")
	}
	if len(normalized) < PrefixMinLength {
		return "", fmt.Errorf("issue prefix must be at least %d characters after normalization", PrefixMinLength)
	}
	return normalized, nil
}

func NormalizeTopicForCreate(input string) (string, error) {
	normalized := NormalizeSlug(input)
	if normalized == "" {
		return "", fmt.Errorf("topic is required")
	}
	if len(normalized) < TopicMinLength {
		return "", fmt.Errorf("topic must be at least %d characters after normalization", TopicMinLength)
	}
	if len(normalized) > TopicMaxLength {
		return "", fmt.Errorf("topic must be at most %d characters after normalization", TopicMaxLength)
	}
	return normalized, nil
}
