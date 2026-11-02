package version

import "testing"

// RaisePolicyOf reads release off Origin: a release raises remotes, and
// nothing built from a working tree does.
func TestRaisePolicyOfReadsOrigin(t *testing.T) {
	t.Parallel()
	if got := RaisePolicyOf(OriginRelease); !got.Release {
		t.Fatalf("RaisePolicyOf(release) = %+v, want Release", got)
	}
	for _, origin := range []string{OriginSource, "snapshot", ""} {
		if got := RaisePolicyOf(origin); got.Release {
			t.Fatalf("RaisePolicyOf(%q) = %+v, want no release", origin, got)
		}
	}
}
