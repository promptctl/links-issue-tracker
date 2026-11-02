package version

import "testing"

// RaisePolicyOf reads release off Origin: a release raises remotes, and
// nothing built from a working tree does.
func TestRaisePolicyOfReadsOrigin(t *testing.T) {
	t.Parallel()
	if got := RaisePolicyOf(Info{FromSource: false}); !got.Release {
		t.Fatalf("RaisePolicyOf(release) = %+v, want Release", got)
	}
	if got := RaisePolicyOf(Info{FromSource: true}); got.Release {
		t.Fatalf("RaisePolicyOf(source) = %+v, want no release", got)
	}
}
