package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNewAttributionAdmitsOnlyCompleteOrAbsent enumerates the four input shapes
// rather than sampling the happy one, because the whole value of this type is
// which inputs it REFUSES: the two half shapes are the ones a corrupted export
// presents, and they are the ones a naive constructor would let through.
func TestNewAttributionAdmitsOnlyCompleteOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stream, workspace string
		wantPresent       bool
	}{
		{"complete pair", "strm123", "ws-1", true},
		{"stream with no workspace to scope it", "strm123", "", false},
		{"workspace naming no producer", "", "ws-1", false},
		{"neither — history predating the feature", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NewAttribution(tc.stream, tc.workspace)
			if got.Present() != tc.wantPresent {
				t.Fatalf("NewAttribution(%q, %q).Present() = %v, want %v",
					tc.stream, tc.workspace, got.Present(), tc.wantPresent)
			}
			// An absent pair must be absent in BOTH halves. Asserting only
			// Present() would pass for a value that kept one half around to leak
			// through an accessor later.
			if !tc.wantPresent && (got.Stream() != "" || got.Workspace() != "") {
				t.Fatalf("absent pair retained content: stream=%q workspace=%q",
					got.Stream(), got.Workspace())
			}
			if tc.wantPresent && (got.Stream() != tc.stream || got.Workspace() != tc.workspace) {
				t.Fatalf("complete pair = (%q, %q), want (%q, %q)",
					got.Stream(), got.Workspace(), tc.stream, tc.workspace)
			}
		})
	}
}

// TestAttributionDecodeCollapsesAHalfPair is the boundary this type exists to
// hold. A sync file or backup is bytes this program did not write, so a
// hand-edited or corrupted one can present a stream with no workspace; decoding
// must yield the absent pair, not a half one that a restore would then persist
// into issue_events as a stream id scoping nothing.
func TestAttributionDecodeCollapsesAHalfPair(t *testing.T) {
	for _, raw := range []string{
		`{"stream":"strm123"}`,
		`{"stream":"strm123","workspace":""}`,
		`{"workspace":"ws-1"}`,
		`{}`,
	} {
		var got Attribution
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", raw, err)
		}
		if got.Present() {
			t.Errorf("Unmarshal(%s) = %+v, want the absent pair", raw, got)
		}
	}
}

// TestAttributionSurvivesARoundTrip pins that sealing the fields did not change
// the wire shape: the halves keep their names, so exports written before this
// change still decode and exports written after are still readable by anything
// that knows the documented format.
func TestAttributionSurvivesARoundTrip(t *testing.T) {
	want := NewAttribution("strm123", "ws-1")
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(encoded) != `{"stream":"strm123","workspace":"ws-1"}` {
		t.Fatalf("wire shape = %s, want the documented stream/workspace object", encoded)
	}
	var got Attribution
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// TestUnattributedEventOmitsAttributionEntirely pins the `omitzero` behavior via
// IsZero: history predating the feature must not gain an empty attribution
// object in every exported record, both because it is noise in a file people
// read and because "the key is absent" is the honest encoding of a fact that was
// never recorded.
func TestUnattributedEventOmitsAttributionEntirely(t *testing.T) {
	encoded, err := json.Marshal(IssueEvent{ID: "evt-1", IssueID: "iss-1", Changes: []FieldChange{}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if bytes := string(encoded); strings.Contains(bytes, "attribution") {
		t.Fatalf("unattributed event carries an attribution key: %s", bytes)
	}
}
