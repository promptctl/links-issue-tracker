package precedence

import "testing"

func TestFirstResolvesOrderedCandidates(t *testing.T) {
	// [LAW:behavior-not-structure] Pins the contract every absorbed callsite
	// (template guidance, sync branch, sync remote) relies on.
	cases := []struct {
		name       string
		candidates []string
		want       string
	}{
		{"first wins over later", []string{"debug", "default"}, "debug"},
		{"empty falls through", []string{"", "default"}, "default"},
		{"all empty resolves empty", []string{"", ""}, ""},
		{"no candidates resolves empty", nil, ""},
		{"whitespace is a value, not emptiness", []string{" ", "default"}, " "},
		{"later non-empty ignored once resolved", []string{"a", "b", "c"}, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := First(tc.candidates...); got != tc.want {
				t.Fatalf("First(%q) = %q, want %q", tc.candidates, got, tc.want)
			}
		})
	}
}
