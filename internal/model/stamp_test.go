package model

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The reason the stored spelling exists: text order must be time order, for
// instants that differ across a whole second, a fractional second and a single
// nanosecond — the three places RFC3339Nano's trimmed zeros misorder.
func TestStampTextOrdersAsTheInstantsDo(t *testing.T) {
	base := time.Date(2026, time.November, 24, 10, 0, 7, 0, time.UTC)
	instants := []time.Time{
		base.Add(-time.Second),
		base,
		base.Add(time.Nanosecond),
		base.Add(100 * time.Millisecond),
		base.Add(500 * time.Millisecond),
		base.Add(999999999 * time.Nanosecond),
		base.Add(time.Second),
		base.Add(10 * time.Second),
	}
	for _, a := range instants {
		for _, b := range instants {
			want := a.Compare(b)
			got := strings.Compare(StampText(a), StampText(b))
			if want != got {
				t.Errorf("order of %s and %s: instants say %d, stored text says %d", a.Format(time.RFC3339Nano), b.Format(time.RFC3339Nano), want, got)
			}
		}
	}
}

// RFC3339Nano is the spelling the stored one replaces; this pins that it
// misorders exactly the pairs the fixed width was chosen to fix, so the test
// above cannot pass by accident.
func TestRFC3339NanoMisordersWhereTheStoredSpellingDoesNot(t *testing.T) {
	whole := time.Date(2026, time.November, 24, 10, 0, 7, 0, time.UTC)
	half := whole.Add(500 * time.Millisecond)
	if !(strings.Compare(whole.Format(time.RFC3339Nano), half.Format(time.RFC3339Nano)) > 0) {
		t.Fatal("expected trimmed RFC3339Nano to sort the whole second after the half second")
	}
	if !(strings.Compare(StampText(whole), StampText(half)) < 0) {
		t.Fatal("stored spelling must sort the whole second before the half second")
	}
}

func TestStampTextIsFixedWidthUTC(t *testing.T) {
	plus2 := time.FixedZone("plus2", 2*60*60)
	for _, instant := range []time.Time{
		time.Date(2026, time.November, 24, 10, 0, 7, 0, time.UTC),
		time.Date(2026, time.November, 24, 10, 0, 7, 1, time.UTC),
		time.Date(2026, time.November, 24, 12, 0, 7, 500, plus2),
	} {
		text := StampText(instant)
		if len(text) != len(StampLayout) {
			t.Errorf("StampText(%s) = %q, width %d, want %d", instant, text, len(text), len(StampLayout))
		}
		if !strings.HasSuffix(text, "Z") {
			t.Errorf("StampText(%s) = %q, want a UTC spelling", instant, text)
		}
	}
	if got, want := StampText(time.Date(2026, time.November, 24, 12, 0, 7, 500, plus2)), "2026-11-24T10:00:07.000000500Z"; got != want {
		t.Errorf("an offset instant spelled %q, want %q", got, want)
	}
}

func TestParseStampReadsEverySpellingTheStoreHasHeld(t *testing.T) {
	want := time.Date(2026, time.November, 24, 10, 0, 7, 500000000, time.UTC)
	for _, spelling := range []string{
		"2026-11-24T10:00:07.5Z",
		"2026-11-24T10:00:07.500000000Z",
		"2026-11-24T12:00:07.5+02:00",
	} {
		got, err := ParseStamp(spelling)
		if err != nil {
			t.Errorf("ParseStamp(%q) error = %v", spelling, err)
			continue
		}
		if !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("ParseStamp(%q) = %s in %s, want %s in UTC", spelling, got, got.Location(), want)
		}
		if StampText(got) != "2026-11-24T10:00:07.500000000Z" {
			t.Errorf("StampText(ParseStamp(%q)) = %q", spelling, StampText(got))
		}
	}
	if _, err := ParseStamp("yesterday"); err == nil {
		t.Error("ParseStamp accepted text that is no timestamp")
	}
}

func TestStampTextSortsAsTheInstantsDoUnderSlicesSort(t *testing.T) {
	base := time.Date(2026, time.November, 24, 10, 0, 7, 0, time.UTC)
	instants := []time.Time{base.Add(time.Second), base.Add(500 * time.Millisecond), base, base.Add(time.Nanosecond)}
	texts := make([]string, len(instants))
	for i, instant := range instants {
		texts[i] = StampText(instant)
	}
	slices.Sort(texts)
	slices.SortFunc(instants, time.Time.Compare)
	for i := range instants {
		if texts[i] != StampText(instants[i]) {
			t.Fatalf("position %d: text sort gave %q, instant sort gave %q", i, texts[i], StampText(instants[i]))
		}
	}
}
