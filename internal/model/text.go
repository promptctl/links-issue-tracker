package model

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Text is free text a ticket stores and a one-row slot prints: a title, a
// lane, a topic, a label, an assignee, an author. Any fmt verb prints it held
// to its row (Format), so a printer holds it by printing it, not by
// remembering to; Raw is the one way to its unheld text, for storage,
// matching and comparison. [LAW:types-are-the-program]
type Text string

// Raw is the text exactly as stored. A renderer never calls it: printing it
// is how a break a one-row slot cannot carry would reach the screen.
func (t Text) Raw() string { return string(t) }

// Format prints the text held to one row whatever the verb and width, so %s,
// %v, %q and %-20s all hold it. [LAW:single-enforcer]
func (t Text) Format(f fmt.State, verb rune) {
	FormatHeld(f, verb, string(t))
}

// ID names a ticket, comment or event: minted by lit, but stored and carried
// by sync and import, so nothing guarantees it holds no break. Any fmt verb
// prints it held to its row, as Text does; it is its own type so a title
// cannot be passed where an id is wanted. [LAW:types-are-the-program]
type ID string

// Raw is the id exactly as stored, for storage, matching and comparison.
func (id ID) Raw() string { return string(id) }

// Trim is the id with surrounding whitespace removed, as a caller typed it.
func (id ID) Trim() ID { return ID(strings.TrimSpace(string(id))) }

// Format prints the id held to one row whatever the verb and width.
func (id ID) Format(f fmt.State, verb rune) {
	FormatHeld(f, verb, string(id))
}

// JoinIDs prints ids joined by sep, each held to its row as %s holds it, so a
// sentence naming several ids holds every one of them.
func JoinIDs(ids []ID, sep string) string {
	held := make([]string, len(ids))
	for i, id := range ids {
		held[i] = fmt.Sprint(id)
	}
	return strings.Join(held, sep)
}

// IDsOf is raw, as a caller typed or a file spelled them, as ids.
func IDsOf(raw []string) []ID {
	ids := make([]ID, len(raw))
	for i, s := range raw {
		ids[i] = ID(s)
	}
	return ids
}

// RawIDs is ids exactly as stored, for a key or a string API.
func RawIDs(ids []ID) []string {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.Raw()
	}
	return raw
}

// FormatHeld writes a value honouring %q and width, so every type that prints
// held formats the same way: held to its row under every verb but %q, which
// Go-quotes the raw value, since its escapes already keep it to one row and
// show exactly which break, tab or invisible the value carries.
func FormatHeld(f fmt.State, verb rune, raw string) {
	held := HoldRow(raw)
	if verb == 'q' {
		flags := ""
		for _, flag := range "+#" {
			if f.Flag(int(flag)) {
				flags += string(flag)
			}
		}
		held = fmt.Sprintf("%"+flags+"q", raw)
	}
	if width, ok := f.Width(); ok {
		if pad := width - utf8.RuneCountInString(held); pad > 0 {
			if f.Flag('-') {
				held += strings.Repeat(" ", pad)
			} else {
				held = strings.Repeat(" ", pad) + held
			}
		}
	}
	_, _ = f.Write([]byte(held))
}

// QuotedBreak stands in for a line break a one-row rendering cannot honour,
// so a multi-line value reads as one token instead of fabricating structure.
// DefusedBreak is what a QuotedBreak already in the text becomes, so wherever
// the token is printed it means a break and nothing else.
const (
	QuotedBreak  = "‹newline›"
	DefusedBreak = "[newline]"
)

// LineBreakNormalizer maps every code point a downstream reader may treat as
// a line break onto the one TextLines splits on. A break this code cannot see
// is a line nothing holds.
var LineBreakNormalizer = strings.NewReplacer(
	"\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n",
	"\u0085", "\n", "\u2028", "\n", "\u2029", "\n",
)

// TextLines splits text at every code point a reader may take for a line
// break and strips each line's invisibles, so no line it returns can move the
// cursor or start a line of its own. It is the one definition of a value's
// printable lines. [LAW:one-source-of-truth]
func TextLines(text string) []string {
	lines := strings.Split(LineBreakNormalizer.Replace(text), "\n")
	for i, line := range lines {
		lines[i] = StripInvisibles(line)
	}
	return lines
}

// HasLineBreak reports whether text holds any code point TextLines splits
// at: the one definition a write gate refuses and a printer holds.
func HasLineBreak(text string) bool {
	return strings.Contains(LineBreakNormalizer.Replace(text), "\n")
}

// HoldRow holds a value to the one row it is printed on: its printable lines
// joined by QuotedBreak, each line's own QuotedBreak defused first so the
// token means only a break, and a tab turned to a space so the value cannot
// add a cell to a tab-separated row. FormatHeld is built on it, and a type
// naming several values renders them raw into FormatHeld rather than calling
// this, so %q still shows their escapes.
func HoldRow(s string) string {
	lines := TextLines(s)
	for i, line := range lines {
		lines[i] = strings.ReplaceAll(strings.ReplaceAll(line, QuotedBreak, DefusedBreak), "\t", " ")
	}
	return strings.Join(lines, QuotedBreak)
}

// StripInvisibles replaces every control (Cc) and format (Cf) rune with
// U+FFFD. The text is printed verbatim to a real terminal, where an ESC
// sequence can repaint or hide a line, and a bidi override reorders one just
// as effectively without being a control rune, which is why the reject set is
// both categories and not a list of code points that drifts as Unicode adds
// more. Tab survives: it moves no cursor. Replaced rather than dropped, so the
// reader sees that something stood there and stripping never joins the text
// on either side into words its author did not write.
//
// Cf costs legitimate rendering: ZWJ and ZWNJ go too, so an emoji sequence or
// Persian text comes out mangled. That is the deliberate trade — a disfigured
// title is cosmetic, a line the reader never sees is not.
func StripInvisibles(line string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !(unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			return r
		}
		return '\uFFFD'
	}, line)
}

// Texts types each stored string as Text.
func Texts(values []string) []Text {
	if values == nil {
		return nil
	}
	out := make([]Text, len(values))
	for i, v := range values {
		out[i] = Text(v)
	}
	return out
}

// RawTexts is each Text exactly as stored, for storage and matching.
func RawTexts(values []Text) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v.Raw()
	}
	return out
}
