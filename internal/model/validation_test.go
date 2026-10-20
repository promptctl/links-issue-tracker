package model

import (
	"errors"
	"fmt"
	"testing"
)

// recordedForTest is an error that records facts and counts and wraps a cause.
type recordedForTest struct {
	facts  map[string]any
	counts map[string]int
	cause  error
}

func (e *recordedForTest) Error() string { return "recorded" }
func (e *recordedForTest) Unwrap() error { return e.cause }
func (e *recordedForTest) EventRecord() (map[string]any, map[string]int) {
	return e.facts, e.counts
}

// The event carries the record of every error in the chain, through wrapping
// and through each branch of a join, and the outermost record wins a key two
// records share.
func TestErrorRecordMergesEveryRecordInTheChain(t *testing.T) {
	t.Parallel()
	err := &recordedForTest{
		facts: map[string]any{"outer": 1, "shared": "outer"},
		cause: errors.Join(
			&recordedForTest{facts: map[string]any{"joined": 2, "shared": "joined"}},
			fmt.Errorf("wrapped: %w", &recordedForTest{facts: map[string]any{"deep": 3}, counts: map[string]int{"deep.count": 4}}),
		),
	}
	facts, counts := ErrorRecord(err)
	for key, want := range map[string]any{"outer": 1, "joined": 2, "deep": 3, "shared": "outer"} {
		if facts[key] != want {
			t.Errorf("facts[%q] = %v, want %v", key, facts[key], want)
		}
	}
	if counts["deep.count"] != 4 {
		t.Errorf("counts = %v, want deep.count 4", counts)
	}
	if facts, counts := ErrorRecord(errors.New("plain")); facts != nil || counts != nil {
		t.Errorf("ErrorRecord(plain) = %v, %v; want nothing recorded", facts, counts)
	}
}
