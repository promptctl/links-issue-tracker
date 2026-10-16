// Package statusline keeps .lit/statusline.json, one file per checkout saying
// what that checkout is working on and what `lit next` would pick for it, so a
// terminal statusline can show both without running lit.
//
// The file costs lit nothing, and that is a property of this package's shape.
// A command records facts it already holds while doing its own work; this
// package folds them into the file after the command has closed its store.
// It takes plain paths and values and imports only the standard library —
// .golangci.yml holds it to that — so a write cannot reach the store, the
// workspace lock, or anything else that waits on another process.
// [LAW:effects-at-boundaries]
package statusline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the version of the file's shape. It changes when a field is
// removed or its meaning changes, never when one is added.
const SchemaVersion = 1

// relPath is where the file sits under a checkout's root.
const relPath = ".lit/statusline.json"

// excludeEntry keeps the file out of git status. .lit/ also holds committed
// project files, so the entry names the file and never the directory. Its
// trailing * covers the temporary sibling a write renames into place, which
// would otherwise show in a git status that runs mid-write.
const excludeEntry = "/" + relPath + "*"

// Ticket is a ticket as the statusline shows it.
type Ticket struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// File is the statusline file's whole content. A nil slot is one this checkout
// holds no current record for: nothing has recorded it yet, or a later command
// invalidated the ticket it named.
type File struct {
	SchemaVersion int        `json:"schema_version"`
	WorkingOn     *WorkingOn `json:"working_on"`
	Next          *Next      `json:"next"`
}

// WorkingOn is the ticket this checkout last started, or that `lit next` last
// handed back to it as its own work in flight.
type WorkingOn struct {
	Ticket Ticket `json:"ticket"`
	// Lane is the progress of the ticket's lane, counted by the command that
	// made the record; nil when that command counted none.
	Lane       *LaneProgress `json:"lane"`
	RecordedAt time.Time     `json:"recorded_at"`
}

// LaneProgress is how far along a lane is: how many of its tickets are
// closed, out of how many it holds — the "N/M done" of a lit backlog claim
// line.
type LaneProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// Next is what the last plain `lit next` in this checkout picked. A nil Ticket
// is a recorded answer, not a missing one: that `lit next` found nothing to
// pick.
type Next struct {
	Ticket     *Ticket   `json:"ticket"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Fact is one thing a command learned about this checkout's work in the course
// of its own job. The set is sealed, and each fact's whole effect on the file
// is its apply method, so what a command can say to the file is this list.
// [LAW:types-are-the-program]
type Fact interface {
	apply(f File, at time.Time) File
}

// Started says this checkout is working on Ticket.
type Started struct{ Ticket Ticket }

// Picked says a plain `lit next` picked Ticket, or found nothing when Ticket is
// nil.
type Picked struct{ Ticket *Ticket }

// Retitled says Ticket's title is now Ticket.Title, so any record naming it
// shows that title from here on; when the record was made does not change.
type Retitled struct{ Ticket Ticket }

// LaneCounted says the lane ticket ID is in stands at Lane, so the record of
// working on ID shows that progress.
type LaneCounted struct {
	ID   string
	Lane LaneProgress
}

// Invalidated says ticket ID has left the state the file's records describe —
// finished, closed, reopened, archived, deleted, or purged — so any record
// naming it no longer stands.
type Invalidated struct{ ID string }

// Replaced says the store the file's records describe was replaced wholesale,
// as a backup restore does, so none of them stands.
type Replaced struct{}

func (s Started) apply(f File, at time.Time) File {
	f.WorkingOn = &WorkingOn{Ticket: s.Ticket, RecordedAt: at}
	return f
}

func (p Picked) apply(f File, at time.Time) File {
	f.Next = &Next{Ticket: p.Ticket, RecordedAt: at}
	return f
}

// Records are copied before they change, never edited in place: the file
// being folded may share them with the one it was folded from.
func (r Retitled) apply(f File, _ time.Time) File {
	if f.WorkingOn != nil && f.WorkingOn.Ticket.ID == r.Ticket.ID {
		working := *f.WorkingOn
		working.Ticket = r.Ticket
		f.WorkingOn = &working
	}
	if f.Next != nil && f.Next.Ticket != nil && f.Next.Ticket.ID == r.Ticket.ID {
		next, ticket := *f.Next, r.Ticket
		next.Ticket = &ticket
		f.Next = &next
	}
	return f
}

func (c LaneCounted) apply(f File, _ time.Time) File {
	if f.WorkingOn != nil && f.WorkingOn.Ticket.ID == c.ID {
		working, lane := *f.WorkingOn, c.Lane
		working.Lane = &lane
		f.WorkingOn = &working
	}
	return f
}

func (i Invalidated) apply(f File, _ time.Time) File {
	if f.WorkingOn != nil && f.WorkingOn.Ticket.ID == i.ID {
		f.WorkingOn = nil
	}
	if f.Next != nil && f.Next.Ticket != nil && f.Next.Ticket.ID == i.ID {
		f.Next = nil
	}
	return f
}

func (Replaced) apply(f File, _ time.Time) File {
	f.WorkingOn, f.Next = nil, nil
	return f
}

// Facts is what one command recorded, in the order it learned it.
type Facts []Fact

// Record appends facts the command has just learned.
func (list *Facts) Record(facts ...Fact) { *list = append(*list, facts...) }

func (list Facts) applyTo(f File, at time.Time) File {
	for _, fact := range list {
		f = fact.apply(f, at)
	}
	return f
}

// Location is where one checkout's file lives, and the git exclude file that
// keeps it out of git status.
type Location struct {
	// File is under the checkout's own root, so every worktree has its own.
	File string
	// Exclude is info/exclude in the git common dir. Every worktree of the
	// repository reads it, so one entry covers them all.
	Exclude string
}

// LocationOf places the file for the checkout rooted at checkoutRoot, in the
// repository whose git common dir is gitCommonDir.
func LocationOf(checkoutRoot, gitCommonDir string) Location {
	return Location{
		File:    filepath.Join(checkoutRoot, filepath.FromSlash(relPath)),
		Exclude: filepath.Join(gitCommonDir, "info", "exclude"),
	}
}

// Outcome is what one Write did. A write that fails part way returns what it
// had done by then: an exclude entry added before the file could not be
// written stays added, and the Outcome says so.
type Outcome struct {
	// Changed names the file's slots, by their field names in the file, whose
	// records this write changed; empty, never nil, when nothing was written.
	Changed []string
	// Bytes is the size of the file written; zero when nothing was written.
	Bytes int
	// Lane is the lane progress the written file's working_on shows; nil when
	// nothing was written, or the file shows no working_on or no count for it.
	Lane *LaneProgress
	// Created is true when the file did not exist before this write.
	Created bool
	// ExcludeAdded is true when this write added the exclude entry.
	ExcludeAdded bool
	// Replaced is why the file already on disk could not be read as this
	// schema and was rewritten from these facts alone; nil when it was read,
	// when there was none, or when the rewrite failed.
	Replaced error
}

// Write folds facts into the checkout's file. A command that recorded no facts
// writes nothing.
//
// It reads the file, applies the facts, and renames a complete new file into
// place, with no lock: a reader sees the old file or the new one, never a
// partial one. Two lit commands finishing in the same checkout at the same
// instant can each read the old file, and the later rename wins, dropping the
// other's facts until that slot is next recorded. Taking a lock to prevent that
// would let a statusline write wait on another process, which is the one cost
// this file must never have.
//
// The exclude entry is added only on the write that creates the file, and
// before the file exists, so the file never shows in git status and every later
// write pays nothing for it.
func Write(loc Location, facts Facts, now time.Time) (Outcome, error) {
	out := Outcome{Changed: []string{}}
	if len(facts) == 0 {
		return out, nil
	}
	prior := File{SchemaVersion: SchemaVersion}
	// before is what the slots held on disk; nil when the file could not be
	// read, so no slot of it is known.
	before := &prior
	var replaced error
	raw, err := os.ReadFile(loc.File)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		out.Created = true
	case err != nil:
		return out, fmt.Errorf("read statusline file: %w", err)
	default:
		parsed, parseErr := parse(raw)
		if parseErr != nil {
			replaced = fmt.Errorf("%s: %w", loc.File, parseErr)
			before = nil
		} else {
			prior = parsed
		}
	}
	if out.Created {
		out.ExcludeAdded, err = ensureExcluded(loc.Exclude)
		if err != nil {
			return out, err
		}
	}
	written := facts.applyTo(prior, now.UTC())
	payload, err := json.MarshalIndent(written, "", "  ")
	if err != nil {
		return out, fmt.Errorf("encode statusline file: %w", err)
	}
	payload = append(payload, '\n')
	if err := replaceFile(loc.File, payload); err != nil {
		return out, err
	}
	out.Replaced = replaced
	out.Changed = changedSlots(before, written)
	out.Bytes = len(payload)
	if written.WorkingOn != nil {
		out.Lane = written.WorkingOn.Lane
	}
	return out, nil
}

// changedSlots names the slots whose records differ between before and after,
// recorded_at included. A file that could not be read (before is nil) has no
// record to match, so every slot is changed: all it shows came from this write.
func changedSlots(before *File, after File) []string {
	changed := []string{}
	if before == nil || !reflect.DeepEqual(before.WorkingOn, after.WorkingOn) {
		changed = append(changed, "working_on")
	}
	if before == nil || !reflect.DeepEqual(before.Next, after.Next) {
		changed = append(changed, "next")
	}
	return changed
}

// parse reads a file this lit wrote. A file of another schema version is not
// one it can merge into: it is replaced, like a damaged one.
func parse(raw []byte) (File, error) {
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return File{}, err
	}
	if f.SchemaVersion != SchemaVersion {
		return File{}, fmt.Errorf("schema_version %d, this lit writes %d", f.SchemaVersion, SchemaVersion)
	}
	return f, nil
}

// ensureExcluded appends the exclude entry unless a line already holds it, and
// reports whether it appended.
func ensureExcluded(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read git exclude file: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	if slices.ContainsFunc(lines, func(line string) bool { return strings.TrimSpace(line) == excludeEntry }) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create git info directory: %w", err)
	}
	separator := ""
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		separator = "\n"
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("open git exclude file: %w", err)
	}
	_, writeErr := file.WriteString(separator + excludeEntry + "\n")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return false, fmt.Errorf("write git exclude file: %w", err)
	}
	return true, nil
}

// replaceFile writes payload to a temporary sibling and renames it over path.
// There is no fsync: the file is rebuilt from the next command's facts, so a
// crash that loses the newest version costs nothing a sync would be worth.
func replaceFile(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create statusline directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create statusline temp file: %w", err)
	}
	_, writeErr := tmp.Write(payload)
	err = errors.Join(writeErr, tmp.Close(), os.Chmod(tmp.Name(), 0o644))
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("write statusline file: %w", err), removeIfPresent(tmp.Name()))
	}
	return nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
