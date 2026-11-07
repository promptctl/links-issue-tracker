package model

import (
	"fmt"
	"testing"
)

// TestTextPrintsHeldUnderEveryVerb pins that printing a Text through fmt is
// what holds it: whichever verb and width a printer picks, a break prints as
// QuotedBreak, a tab as a space and an ESC as U+FFFD, so no printer can forget.
// %q Go-quotes the stored text, whose escapes already keep it to one row.
func TestTextPrintsHeldUnderEveryVerb(t *testing.T) {
	t.Parallel()
	text := Text("a\u2028b\tc\x1bd")
	held := "a" + QuotedBreak + "b c\uFFFDd"
	for _, tc := range []struct{ format, want string }{
		{"%s", held},
		{"%v", held},
		{"%q", fmt.Sprintf("%q", string(text))},
		{"[%-16s]", "[" + held + " ]"},
		{"[%17s]", "[  " + held + "]"},
	} {
		if got := fmt.Sprintf(tc.format, text); got != tc.want {
			t.Errorf("Sprintf(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
	if got := text.Raw(); got != string(text) {
		t.Errorf("Raw() = %q, want the stored text", got)
	}
}

// TestFormatCompositeHonoursQAndWidth pins that a type rendering several held
// values formats as one held value does: %q quotes the rendering, width pads.
func TestFormatCompositeHonoursQAndWidth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ format, want string }{
		{"%v", "a (via epic)"},
		{"%q", `"a (via epic)"`},
		{"[%-14v]", "[a (via epic)  ]"},
		{"[%14v]", "[  a (via epic)]"},
	} {
		if got := fmt.Sprintf(tc.format, composite("a (via epic)")); got != tc.want {
			t.Errorf("Sprintf(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}

type composite string

func (c composite) Format(f fmt.State, verb rune) { FormatComposite(f, verb, string(c)) }

// TestIDQuotesRawUnderQ pins that %q shows an id exactly as given, escapes and
// all, so an error naming a rejected id shows the tab or break it carried,
// while %s holds it to its row as Text does.
func TestIDQuotesRawUnderQ(t *testing.T) {
	t.Parallel()
	id := ID("a\nb\tc")
	for _, tc := range []struct{ format, want string }{
		{"%s", "a" + QuotedBreak + "b c"},
		{"%q", `"a\nb\tc"`},
	} {
		if got := fmt.Sprintf(tc.format, id); got != tc.want {
			t.Errorf("Sprintf(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}
