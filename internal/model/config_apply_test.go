package model

import (
	"testing"
	"time"
)

func TestTheConfigInForceIsTheNewestApply(t *testing.T) {
	at := func(seconds int) time.Time { return time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC) }
	older := ConfigApply{ID: "cfg-b", Content: "old", CreatedAt: at(1)}
	newer := ConfigApply{ID: "cfg-a", Content: "new", CreatedAt: at(2)}
	tiedLow := ConfigApply{ID: "cfg-c", Content: "tied low", CreatedAt: at(3)}
	tiedHigh := ConfigApply{ID: "cfg-d", Content: "tied high", CreatedAt: at(3)}

	if _, applied := ConfigInForce(nil); applied {
		t.Error("no applies has a config in force; want the defaults")
	}
	for _, tc := range []struct {
		name    string
		applies []ConfigApply
		want    ConfigApply
	}{
		{"one", []ConfigApply{older}, older},
		{"the newest instant, whatever the order", []ConfigApply{newer, older}, newer},
		{"one instant, the greater id", []ConfigApply{tiedHigh, older, tiedLow}, tiedHigh},
	} {
		got, applied := ConfigInForce(tc.applies)
		if !applied || got != tc.want {
			t.Errorf("%s: in force = (%+v, %v), want %+v", tc.name, got, applied, tc.want)
		}
	}
}

func TestAnApplyIsNamedByItsContent(t *testing.T) {
	a := ConfigApply{ID: "cfg-1", Content: "[ready]\n"}
	b := ConfigApply{ID: "cfg-2", Content: "[ready]\n"}
	if a.Digest() != b.Digest() || a.Digest() != DigestConfig("[ready]\n") {
		t.Errorf("one content has digests %s and %s", a.Digest(), b.Digest())
	}
	if a.Digest() == DigestConfig("") {
		t.Error("two contents share a digest")
	}
}
