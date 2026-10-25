package storage

import (
	"sync"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// StampTally is where an engine reads the instant a write stamps, and the
// record of how often history held that instant back from the clock.
//
// The instant is [model.NextEventInstant] over the history the write was
// planned against: the clock's reading, unless that history already holds an
// event at or past it. Both engines embed one, so the rule and the account of
// it have one home whichever engine wrote. [LAW:single-enforcer]
//
// It is the engine's own state behind this API, guarded because a store may
// be written from more than one goroutine. [LAW:no-shared-mutable-globals]
type StampTally struct {
	mu        sync.Mutex
	corrected StampCorrections
}

// Stamp is the instant a write planned against seen takes when clock is the
// clock it reads.
func (t *StampTally) Stamp(clock Clock, seen []model.IssueEvent) time.Time {
	now := clock.Now()
	at := model.NextEventInstant(now, seen)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.corrected = t.corrected.after(at.Sub(now))
	return at
}

// StampCorrections reports every stamp this tally has handed out that history
// held back from the clock.
func (t *StampTally) StampCorrections() StampCorrections {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.corrected
}

// StampCorrections is how far behind the history it was planned against the
// clock was, over a store's stamps: how many stamps history held back, and
// the largest step any of them took past the clock's reading. A clone whose
// clock runs behind the clones it syncs with shows here, and not only in
// event order nobody is looking at. [LAW:nothing-unseen]
type StampCorrections struct {
	Count int
	Max   time.Duration
}

// after is the corrections once one more stamp has stepped correction past
// its clock's reading, zero for a stamp the clock already put past history.
func (c StampCorrections) after(correction time.Duration) StampCorrections {
	if correction == 0 {
		return c
	}
	return StampCorrections{Count: c.Count + 1, Max: max(c.Max, correction)}
}

// EventRecord is the corrections as event facts. Zero is recorded as zero, so
// a command whose clock never ran behind says so rather than saying nothing.
func (c StampCorrections) EventRecord() (facts map[string]any, counts map[string]int) {
	return map[string]any{"store.stamp_correction_max_ns": c.Max.Nanoseconds()},
		map[string]int{"store.stamps_corrected": c.Count}
}

// StampReporter is the account an engine gives of the stamps it handed out.
type StampReporter interface {
	StampCorrections() StampCorrections
}
