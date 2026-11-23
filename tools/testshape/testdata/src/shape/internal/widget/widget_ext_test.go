package widget_test

import (
	"testing"

	"example.com/shape/internal/widget"
)

func TestFromOutsideReachesGitThroughProductionCode(t *testing.T) {
	if _, err := widget.Describe("."); err != nil {
		t.Fatal(err)
	}
}
