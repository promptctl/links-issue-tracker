package historywindow

import (
	"fmt"
	"strings"
	"testing"
)

// A history window keeps at least the head and drops at least one commit
// whenever it fires, so New builds exactly those windows and
// refuses every other pair, naming both numbers.
func TestNewBuildsOnlyAWindowThatKeepsAndDrops(t *testing.T) {
	for _, tc := range []struct {
		reRootAt, keep uint64
		legal          bool
	}{
		{5, 2, true},
		{2, 1, true},
		{5, 0, false},
		{5, 5, false},
		{5, 7, false},
	} {
		window, err := New(tc.reRootAt, tc.keep)
		if !tc.legal {
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("re_root_at %d, keep %d", tc.reRootAt, tc.keep)) {
				t.Errorf("New(%d, %d) = %+v, %v; want a refusal naming both numbers", tc.reRootAt, tc.keep, window, err)
			}
			continue
		}
		if err != nil || window.ReRootAt() != tc.reRootAt || window.Keep() != tc.keep {
			t.Errorf("New(%d, %d) = %+v, %v; want that window", tc.reRootAt, tc.keep, window, err)
		}
	}
}

// The zero Window is no window, so reading it is a defect, reported as one.
func TestTheZeroWindowIsADefectToRead(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("reading Window{} did not panic; want the zero value refused")
		}
	}()
	_ = Window{}.ReRootAt()
}
