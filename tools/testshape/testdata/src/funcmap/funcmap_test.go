package funcmap

import "testing"

// Every entry of the map is reachable from a use of it, so the plain entry
// is charged the git one's touch.
func TestAMapOfFuncsReachesEveryEntry(t *testing.T) { _ = Format("plain") }
