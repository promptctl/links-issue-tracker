package storage

import (
	"sync"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// StampTally is where an engine reads the instant a write stamps, and the
// record of how often history held that reading back from the clock.
//
// The instant is [model.NextEventInstant] over the newest instant in the
// history the write was planned against: the clock's reading, unless that
// history already holds an event at or past it. Both engines embed one, so the rule and the account of
// it have one home whichever engine wrote. [LAW:single-enforcer]
//
// It is the engine's own state behind this API, guarded because a store may
// be written from more than one goroutine. [LAW:no-shared-mutable-globals]
type StampTally struct {
	mu        sync.Mutex
	corrected StampCorrections
}

// Stamp is the instant a write takes when clock is the clock it reads and
// newest is the newest instant in the history it was planned against.
func (t *StampTally) Stamp(clock Clock, newest time.Time) time.Time {
	now := clock.Now()
	at := model.NextEventInstant(now, newest)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.corrected = t.corrected.after(at.Sub(now))
	return at
}

// StampCorrections reports every clock reading this tally has taken that
// history held back.
func (t *StampTally) StampCorrections() StampCorrections {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.corrected
}

// StampCorrections is how far behind the history a write was planned against
// the clock was, over a store's readings: how many readings history held back,
// and the largest step any of them took past the clock. A reading is evidence
// of the clock's skew whether or not the write it was taken for lands, so a
// refused or no-op write counts too. A clone whose clock runs behind the
// clones it syncs with shows here, and not only in event order nobody is
// looking at. [LAW:nothing-unseen]
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

// EventValue is one store's corrections as an event occurrence. Zero is
// recorded as zero, so a store whose clock never ran behind says so rather
// than saying nothing.
func (c StampCorrections) EventValue() map[string]any {
	return map[string]any{"store.stamps_corrected": c.Count, "store.stamp_correction_max_ns": c.Max.Nanoseconds()}
}

// StampReporter is the account an engine gives of the stamps it handed out.
type StampReporter interface {
	StampCorrections() StampCorrections
}
