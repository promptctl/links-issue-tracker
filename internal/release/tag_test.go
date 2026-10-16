package release

import (
	"strings"
	"testing"
)

func mustTag(t *testing.T, s string) Tag {
	t.Helper()
	tag, err := ParseTag(s)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

// A Tag's String is the URL path segment its assets live under, so every
// string ParseTag refuses here would otherwise have reached a manifest URL:
// a non-release, a URL metacharacter, whitespace, a separator, or traversal.
// Which tags are releases is held to release-tags.sh by
// TestReleaseTagParsesExactlyTheReleaseTags (scripts/install_test.go).
func TestParseTag(t *testing.T) {
	t.Parallel()
	if got := mustTag(t, "v0.4.1").String(); got != "v0.4.1" {
		t.Errorf("ParseTag(v0.4.1).String() = %q; want v0.4.1", got)
	}
	for _, bad := range []string{
		"0.4.1",
		"v0.14.0.1",
		"v0.4.1?inject=1",
		"v0.4.1#frag",
		"v0.4.1%2F..",
		"v0.4.1 ",
		"v0.4.1\n",
		"v",
		"v..",
		"v/0.1",
	} {
		if _, err := ParseTag(bad); err == nil || !strings.Contains(err.Error(), "is not a release tag") {
			t.Errorf("ParseTag(%q) err = %v; want a not-a-release-tag refusal", bad, err)
		}
	}
}
