package runfacts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/deadline"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

var released = version.SchemaCeiling{Max: 7, Source: "stamped", Bound: version.BoundReleased}

// Of builds a run's facts only from built deadlines and the released bound.
func TestOfRefusesARunMissingAFact(t *testing.T) {
	t.Parallel()
	if _, err := Of(deadline.Deadlines{}, released, version.RaisePolicy{}); err == nil || !strings.Contains(err.Error(), "no deadlines") {
		t.Errorf("Of(zero deadlines) = %v, want it refused as carrying none", err)
	}
	for _, bound := range []version.SchemaBound{"", version.BoundHold, version.BoundNone} {
		ceiling := released
		ceiling.Bound = bound
		if _, err := Of(deadline.Shipped(), ceiling, version.RaisePolicy{}); err == nil || !strings.Contains(err.Error(), `must be the "released" bound`) {
			t.Errorf("Of(bound %q) = %v, want it refused as not the released bound", bound, err)
		}
	}
}

// What MarshalJSON writes Parse reads back as the same facts, a non-release's
// policy included.
func TestHandedFactsReadBackWhole(t *testing.T) {
	t.Parallel()
	for _, policy := range []version.RaisePolicy{{Release: true}, {Release: false}} {
		facts, err := Of(deadline.Shipped(), released, policy)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(data)
		if err != nil || got != facts {
			t.Errorf("Parse(%s) = %+v, %v; want %+v", data, got, err, facts)
		}
		var decoded Facts
		if err := json.Unmarshal(data, &decoded); err != nil || decoded != facts {
			t.Errorf("json.Unmarshal(%s) = %+v, %v; want %+v", data, decoded, err, facts)
		}
	}
}

// Bytes that carry no facts are refused, not read as the zero Facts.
func TestParseRefusesNoFacts(t *testing.T) {
	t.Parallel()
	for _, data := range []string{"", "{}"} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%q) succeeded; want it refused", data)
		}
	}
	// Decoding Facts as a field reads it through Parse too.
	for _, data := range []string{`{"facts":{}}`, `{"facts":null}`} {
		var into struct {
			Facts Facts `json:"facts"`
		}
		if err := json.Unmarshal([]byte(data), &into); err == nil {
			t.Errorf("json.Unmarshal(%s) succeeded with %+v; want it refused as Parse refuses it", data, into.Facts)
		}
	}
}

// The zero Facts is no run's: reading a fact from it panics.
func TestTheZeroFactsCannotBeRead(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("reading the zero Facts' deadlines did not panic")
		}
	}()
	_ = Facts{}.Deadlines()
}
