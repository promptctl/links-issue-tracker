package release

import (
	"fmt"
	"regexp"
)

// tagPattern is scripts/release-tags.sh's RELEASE_TAG_ERE: a release tag is
// exactly vMAJOR.MINOR.PATCH, the shape release-validate.yml cuts. origin also
// carries v0.2.0.1-shaped tags — GitHub releases with no assets — and matching
// the shape exactly is what keeps them from reaching a manifest download.
//
// [LAW:one-source-of-truth] exception: release-tags.sh is the definition; Go
// cannot source it, so this copies it, and TestReleaseTagParsesExactlyTheReleaseTags
// (scripts/install_test.go) holds the copy to the same accept-set.
var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Tag is a release tag that has been parsed: holding one is the proof that it
// names a release, so a manifest URL built from it is a single, safe path
// segment.
//
// [LAW:parse-dont-validate] ParseTag is the only way to make a non-zero Tag,
// whichever door the string arrives through — a user's --to or the release feed.
type Tag struct{ s string }

// ParseTag accepts s exactly when release-tags.sh calls it a release tag.
func ParseTag(s string) (Tag, error) {
	if !tagPattern.MatchString(s) {
		return Tag{}, fmt.Errorf("%q is not a release tag (a release tag is vX.Y.Z, e.g. v0.4.1)", s)
	}
	return Tag{s: s}, nil
}

// String returns the tag as published, v-prefixed: the URL path segment the
// release's assets live under.
func (t Tag) String() string { return t.s }
