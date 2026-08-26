package issueid

import "testing"

func TestNormalizeSlug(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"lowercases", "Fix Parser", "fix-parser"},
		{"trims surrounding whitespace", "  spaced  ", "spaced"},
		{"collapses runs of punctuation to one dash", "a---b___c", "a-b-c"},
		{"collapses mixed non-alnum runs", "a!!@@##b", "a-b"},
		{"trims leading and trailing dashes", "--edge--", "edge"},
		{"keeps digits", "v2 release", "v2-release"},
		{"all punctuation normalizes to empty", "!!!", ""},
		{"empty input normalizes to empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeSlug(tc.input)
			if got != tc.want {
				t.Errorf("NormalizeSlug(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeConfiguredPrefix(t *testing.T) {
	t.Run("valid prefix passes through normalized", func(t *testing.T) {
		got, err := NormalizeConfiguredPrefix("Links")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "links" {
			t.Errorf("got %q, want %q", got, "links")
		}
	})

	t.Run("empty after normalization is rejected", func(t *testing.T) {
		if _, err := NormalizeConfiguredPrefix("!!!"); err == nil {
			t.Fatal("expected error for empty-after-normalization prefix, got nil")
		}
	})

	t.Run("shorter than minimum is rejected", func(t *testing.T) {
		if _, err := NormalizeConfiguredPrefix("ab"); err == nil {
			t.Fatal("expected error for too-short prefix, got nil")
		}
	})

	t.Run("exactly minimum length is accepted", func(t *testing.T) {
		got, err := NormalizeConfiguredPrefix("abc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "abc" {
			t.Errorf("got %q, want %q", got, "abc")
		}
	})

	t.Run("longer than maximum is truncated", func(t *testing.T) {
		got, err := NormalizeConfiguredPrefix("thisprefixiswaytoolong")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) > PrefixMaxLength {
			t.Errorf("got %q of length %d, want at most %d", got, len(got), PrefixMaxLength)
		}
		if got != "thisprefixis" {
			t.Errorf("got %q, want %q", got, "thisprefixis")
		}
	})

	t.Run("truncation landing on a dash trims it", func(t *testing.T) {
		// "prefix-that-goes-long" truncated to 12 chars is "prefix-that-",
		// which must have its trailing dash trimmed.
		got, err := NormalizeConfiguredPrefix("prefix-that-goes-long")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) == 0 || got[len(got)-1] == '-' {
			t.Errorf("got %q, want no trailing dash", got)
		}
	})
}

func TestNormalizeTopicForCreate(t *testing.T) {
	t.Run("valid topic passes through normalized", func(t *testing.T) {
		got, err := NormalizeTopicForCreate("Bug Fixes")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "bug-fixes" {
			t.Errorf("got %q, want %q", got, "bug-fixes")
		}
	})

	t.Run("empty after normalization is rejected", func(t *testing.T) {
		if _, err := NormalizeTopicForCreate("###"); err == nil {
			t.Fatal("expected error for empty-after-normalization topic, got nil")
		}
	})

	t.Run("shorter than minimum is rejected", func(t *testing.T) {
		if _, err := NormalizeTopicForCreate("ab"); err == nil {
			t.Fatal("expected error for too-short topic, got nil")
		}
	})

	t.Run("exactly minimum length is accepted", func(t *testing.T) {
		got, err := NormalizeTopicForCreate("abc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "abc" {
			t.Errorf("got %q, want %q", got, "abc")
		}
	})

	t.Run("longer than maximum is rejected", func(t *testing.T) {
		tooLong := "this-topic-is-far-too-long-to-be-accepted-here"
		if len(tooLong) <= TopicMaxLength {
			t.Fatalf("test fixture too short: %d <= %d", len(tooLong), TopicMaxLength)
		}
		if _, err := NormalizeTopicForCreate(tooLong); err == nil {
			t.Fatal("expected error for too-long topic, got nil")
		}
	})

	t.Run("exactly maximum length is accepted", func(t *testing.T) {
		exact := "abcdefghijklmnopqrstuvwxyzabcd" // 30 chars, matches TopicMaxLength
		if len(exact) != TopicMaxLength {
			t.Fatalf("test fixture length %d, want %d", len(exact), TopicMaxLength)
		}
		got, err := NormalizeTopicForCreate(exact)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != exact {
			t.Errorf("got %q, want %q", got, exact)
		}
	})
}
