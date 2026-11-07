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

// TestCompositeFormatsAsOneHeldValue pins that a type naming several values
// renders them raw into FormatHeld and so formats as one held value does: %v
// holds the whole row and %q shows the raw escapes of every part.
func TestCompositeFormatsAsOneHeldValue(t *testing.T) {
	t.Parallel()
	c := composite{id: ID("a\tb"), tail: "x\ny"}
	for _, tc := range []struct{ format, want string }{
		{"%v", "a b x" + QuotedBreak + "y"},
		{"%q", `"a\tb x\ny"`},
	} {
		if got := fmt.Sprintf(tc.format, c); got != tc.want {
			t.Errorf("Sprintf(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}

type composite struct {
	id   ID
	tail string
}

func (c composite) Format(f fmt.State, verb rune) { FormatHeld(f, verb, c.id.Raw()+" "+c.tail) }

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
