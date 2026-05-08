package cli

import (
	"regexp"
	"testing"
)

func TestApplyTokenIsDeterministicAndStable(t *testing.T) {
	a := applyToken("transition", "done", "test-1", "2026-05-08T00:00:00Z")
	b := applyToken("transition", "done", "test-1", "2026-05-08T00:00:00Z")
	if a != b {
		t.Fatalf("identical inputs produced different tokens: %q vs %q", a, b)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(a) {
		t.Fatalf("token %q is not 8 hex chars", a)
	}
}

func TestApplyTokenChangesWithAnyPart(t *testing.T) {
	base := applyToken("transition", "done", "test-1", "2026-05-08T00:00:00Z")
	cases := [][]string{
		{"transition", "close", "test-1", "2026-05-08T00:00:00Z"},
		{"transition", "done", "test-2", "2026-05-08T00:00:00Z"},
		{"transition", "done", "test-1", "2026-05-08T00:00:01Z"},
		{"transition", "done", "test-1"}, // missing fingerprint
	}
	for _, c := range cases {
		if got := applyToken(c...); got == base {
			t.Fatalf("applyToken(%v) = %q, expected to differ from base %q", c, got, base)
		}
	}
}

func TestApplyTokenResistsBoundaryCollision(t *testing.T) {
	// Without the NUL separator, ["a", "bc"] and ["ab", "c"] would collide.
	a := applyToken("a", "bc")
	b := applyToken("ab", "c")
	if a == b {
		t.Fatalf("boundary collision: applyToken(\"a\",\"bc\") == applyToken(\"ab\",\"c\") == %q", a)
	}
}
