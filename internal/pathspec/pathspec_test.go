package pathspec

import (
	"path/filepath"
	"testing"
)

// The PathSpec contract: trimmed at construction, absence is the zero value,
// absence propagates through Or and Join. [LAW:behavior-not-structure]
func TestNewTrims(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"\t\n", ""},
		{"/a/b", "/a/b"},
		{"  /a/b  ", "/a/b"},
	}
	for _, c := range cases {
		if got := New(c.raw).String(); got != c.want {
			t.Errorf("New(%q).String() = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestIsEmpty(t *testing.T) {
	if !New("  ").IsEmpty() {
		t.Error("whitespace-only input must be absent")
	}
	if (PathSpec{}).IsEmpty() != true {
		t.Error("zero value must be absent")
	}
	if New("/a").IsEmpty() {
		t.Error("present path must not be absent")
	}
}

func TestOr(t *testing.T) {
	fallback := New("/fallback")
	if got := New("").Or(fallback); got != fallback {
		t.Errorf("absent.Or(fallback) = %q, want %q", got, fallback)
	}
	present := New("/present")
	if got := present.Or(fallback); got != present {
		t.Errorf("present.Or(fallback) = %q, want %q", got, present)
	}
}

func TestJoinPropagatesAbsence(t *testing.T) {
	if got := New("").Join("x", "y"); !got.IsEmpty() {
		t.Errorf("absent.Join must stay absent, got %q", got)
	}
	want := filepath.Join("/root", "x", "y")
	if got := New("/root").Join("x", "y").String(); got != want {
		t.Errorf("Join = %q, want %q", got, want)
	}
}
