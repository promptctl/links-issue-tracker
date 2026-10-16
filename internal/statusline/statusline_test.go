package statusline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	alpha = Ticket{ID: "t-alpha", Title: "Alpha"}
	bravo = Ticket{ID: "t-bravo", Title: "Bravo"}
	// alphaRenamed is alpha after a retitle.
	alphaRenamed = Ticket{ID: "t-alpha", Title: "Alpha, renamed"}
	at           = time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	lane         = Lane{Done: 1, Total: 4}
)

func testLocation(t *testing.T) Location {
	t.Helper()
	root := t.TempDir()
	return LocationOf(filepath.Join(root, "checkout"), filepath.Join(root, "common"))
}

func readFile(t *testing.T, loc Location) File {
	t.Helper()
	raw, err := os.ReadFile(loc.File)
	if err != nil {
		t.Fatalf("read statusline file: %v", err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("statusline file is not JSON: %v\n%s", err, raw)
	}
	return f
}

func write(t *testing.T, loc Location, facts ...Fact) Outcome {
	t.Helper()
	out, err := Write(loc, facts, at)
	if err != nil {
		t.Fatalf("Write(%v) error = %v", facts, err)
	}
	return out
}

// Each write folds its facts into what earlier writes left, so the file
// carries every slot's latest record whichever command recorded it.
func TestWritesFoldFactsIntoTheFile(t *testing.T) {
	working := func(ticket Ticket) *WorkingOn { return &WorkingOn{Ticket: ticket, RecordedAt: at} }
	counted := func(ticket Ticket) *WorkingOn { return &WorkingOn{Ticket: ticket, Lane: &lane, RecordedAt: at} }
	next := func(ticket *Ticket) *Next { return &Next{Ticket: ticket, RecordedAt: at} }
	cases := []struct {
		name   string
		writes [][]Fact
		want   File
	}{
		{
			name:   "a start records what the checkout is working on",
			writes: [][]Fact{{Started{alpha}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha)},
		},
		{
			name:   "a later write keeps the slots it says nothing about",
			writes: [][]Fact{{Started{alpha}}, {Picked{&bravo}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha), Next: next(&bravo)},
		},
		{
			name:   "a pick of nothing is recorded, not left blank",
			writes: [][]Fact{{Picked{&alpha}}, {Picked{nil}}},
			want:   File{SchemaVersion: SchemaVersion, Next: next(nil)},
		},
		{
			name:   "a second start replaces the first",
			writes: [][]Fact{{Started{alpha}}, {Started{bravo}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo)},
		},
		{
			name:   "invalidating a ticket clears every slot naming it",
			writes: [][]Fact{{Started{alpha}, Picked{&alpha}}, {Invalidated{alpha.ID}}},
			want:   File{SchemaVersion: SchemaVersion},
		},
		{
			name:   "invalidating another ticket clears nothing",
			writes: [][]Fact{{Started{alpha}, Picked{&bravo}}, {Invalidated{"t-other"}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha), Next: next(&bravo)},
		},
		{
			name:   "invalidating the pick leaves the work in flight",
			writes: [][]Fact{{Started{alpha}, Picked{&bravo}}, {Invalidated{bravo.ID}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha)},
		},
		{
			name:   "a retitle renames every slot naming the ticket",
			writes: [][]Fact{{Started{alpha}, Picked{&alpha}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alphaRenamed), Next: next(&alphaRenamed)},
		},
		{
			name:   "retitling another ticket renames nothing",
			writes: [][]Fact{{Started{alpha}, Picked{&bravo}}, {Retitled{Ticket{ID: "t-other", Title: "Other"}}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha), Next: next(&bravo)},
		},
		{
			name:   "retitling leaves a recorded pick of nothing alone",
			writes: [][]Fact{{Picked{nil}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, Next: next(nil)},
		},
		{
			name:   "a lane count shows on the work it names",
			writes: [][]Fact{{Started{alpha}, LaneCounted{alpha.ID, lane}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: counted(alpha)},
		},
		{
			name:   "a lane count for another ticket shows nowhere",
			writes: [][]Fact{{Started{alpha}, Picked{&bravo}, LaneCounted{bravo.ID, lane}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha), Next: next(&bravo)},
		},
		{
			name:   "a retitle keeps the lane count",
			writes: [][]Fact{{Started{alpha}, LaneCounted{alpha.ID, lane}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: counted(alphaRenamed)},
		},
		{
			name:   "a new start drops the last ticket's lane count",
			writes: [][]Fact{{Started{alpha}, LaneCounted{alpha.ID, lane}}, {Started{bravo}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo)},
		},
		{
			name:   "facts apply in the order the command learned them",
			writes: [][]Fact{{Started{alpha}, Invalidated{alpha.ID}, Started{bravo}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc := testLocation(t)
			for _, facts := range tc.writes {
				write(t, loc, facts...)
			}
			if got := readFile(t, loc); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("file = %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// A retitle changes what a record shows, not when the checkout made it.
func TestARetitleKeepsWhenTheRecordWasMade(t *testing.T) {
	loc := testLocation(t)
	write(t, loc, Started{alpha}, Picked{&alpha})
	if _, err := Write(loc, Facts{Retitled{alphaRenamed}}, at.Add(time.Hour)); err != nil {
		t.Fatalf("Write(retitle) error = %v", err)
	}
	got := readFile(t, loc)
	if !got.WorkingOn.RecordedAt.Equal(at) || !got.Next.RecordedAt.Equal(at) {
		t.Fatalf("recorded_at after a retitle = %v / %v, want both %v", got.WorkingOn.RecordedAt, got.Next.RecordedAt, at)
	}
}

// The field names and nulls are what an external statusline script reads, so
// they are pinned as bytes, not only as a round trip through this package's
// own types.
func TestFileEncodesTheDocumentedShape(t *testing.T) {
	loc := testLocation(t)
	write(t, loc, Started{alpha}, LaneCounted{alpha.ID, lane}, Picked{nil})
	raw, err := os.ReadFile(loc.File)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"schema_version": float64(1),
		"working_on": map[string]any{
			"ticket":      map[string]any{"id": "t-alpha", "title": "Alpha"},
			"lane":        map[string]any{"done": float64(1), "total": float64(4)},
			"recorded_at": "2026-10-15T09:30:00Z",
		},
		"next": map[string]any{"ticket": nil, "recorded_at": "2026-10-15T09:30:00Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("file = %s\nwant %v", raw, want)
	}
}

func TestACommandWithNoFactsWritesNothing(t *testing.T) {
	loc := testLocation(t)
	out, err := Write(loc, nil, at)
	if want := (Outcome{Changed: []string{}}); err != nil || !reflect.DeepEqual(out, want) {
		t.Fatalf("Write(no facts) = %+v, %v; want %+v", out, err, want)
	}
	for _, path := range []string{loc.File, loc.Exclude} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s exists after a write with no facts (stat err = %v)", path, err)
		}
	}
}

// The exclude entry is written by the write that creates the file and by no
// other, and never twice.
func TestTheCreatingWriteAddsTheExcludeEntryOnce(t *testing.T) {
	loc := testLocation(t)
	first := write(t, loc, Started{alpha})
	if !first.Created || !first.ExcludeAdded || first.Bytes == 0 {
		t.Fatalf("first write = %+v; want Created, ExcludeAdded and a byte count", first)
	}
	// Only the creating write reads or writes the exclude file: an entry
	// removed after it stays removed through every later write.
	if err := os.WriteFile(loc.Exclude, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	second := write(t, loc, Picked{&bravo})
	if second.Created || second.ExcludeAdded {
		t.Fatalf("second write = %+v; want neither Created nor ExcludeAdded", second)
	}
	if raw, err := os.ReadFile(loc.Exclude); err != nil || len(raw) != 0 {
		t.Fatalf("a write that did not create the file touched the exclude file: %q, %v", raw, err)
	}
	// Recreating the file restores the missing entry, and a creation that
	// finds the entry present leaves it single.
	for _, wantAdded := range []bool{true, false} {
		if err := os.Remove(loc.File); err != nil {
			t.Fatal(err)
		}
		recreated := write(t, loc, Started{alpha})
		if !recreated.Created || recreated.ExcludeAdded != wantAdded {
			t.Fatalf("write after the file was removed = %+v; want Created, ExcludeAdded=%v", recreated, wantAdded)
		}
	}
	raw, err := os.ReadFile(loc.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), excludeEntry); got != 1 {
		t.Fatalf("exclude file holds the entry %d times, want 1:\n%s", got, raw)
	}
}

// An exclude file someone else wrote keeps its lines, and the entry lands on
// a line of its own even when the last existing line has no newline.
func TestTheExcludeEntryIsAppendedOnItsOwnLine(t *testing.T) {
	loc := testLocation(t)
	if err := os.MkdirAll(filepath.Dir(loc.Exclude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loc.Exclude, []byte("# local\n*.swp"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, loc, Started{alpha})
	raw, err := os.ReadFile(loc.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# local\n*.swp\n" + excludeEntry + "\n"; string(raw) != want {
		t.Fatalf("exclude file = %q, want %q", raw, want)
	}
}

// Each write names the slots whose records it changed, and only those. A slot
// its facts did not reach is not changed; one re-recorded with the ticket it
// already held is, because its recorded_at moved.
func TestAWriteNamesTheSlotsItChanged(t *testing.T) {
	loc := testLocation(t)
	for i, step := range []struct {
		facts []Fact
		want  []string
	}{
		{[]Fact{Started{alpha}}, []string{"working_on"}},
		{[]Fact{Picked{&bravo}}, []string{"next"}},
		{[]Fact{Started{alpha}}, []string{"working_on"}},
		{[]Fact{Retitled{Ticket{ID: "t-other", Title: "Other"}}}, []string{}},
		{[]Fact{Invalidated{"t-other"}}, []string{}},
		{[]Fact{Retitled{alphaRenamed}}, []string{"working_on"}},
		{[]Fact{Invalidated{bravo.ID}}, []string{"next"}},
		{[]Fact{Started{bravo}, Picked{&alpha}}, []string{"working_on", "next"}},
		{[]Fact{LaneCounted{bravo.ID, lane}}, []string{"working_on"}},
		{[]Fact{LaneCounted{bravo.ID, lane}}, []string{}},
		{[]Fact{Replaced{}}, []string{"working_on", "next"}},
		{[]Fact{Replaced{}}, []string{}},
	} {
		out, err := Write(loc, step.facts, at.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out.Changed, step.want) {
			t.Fatalf("Write(%v).Changed = %q, want %q", step.facts, out.Changed, step.want)
		}
	}
}

// Each write reports the lane count the working_on it wrote shows, whichever
// command counted it, and nothing when that record shows none or is null.
func TestAWriteReportsTheLaneCountItsWorkingOnShows(t *testing.T) {
	loc := testLocation(t)
	for _, step := range []struct {
		facts []Fact
		want  *Lane
	}{
		{[]Fact{Picked{&bravo}}, nil},
		{[]Fact{Started{alpha}}, nil},
		{[]Fact{LaneCounted{alpha.ID, lane}}, &lane},
		{[]Fact{Retitled{alphaRenamed}}, &lane},
		{[]Fact{Invalidated{alpha.ID}}, nil},
	} {
		out := write(t, loc, step.facts...)
		if !reflect.DeepEqual(out.Lane, step.want) {
			t.Fatalf("Write(%v).Lane = %v, want %v", step.facts, out.Lane, step.want)
		}
	}
}

// A write that fails after adding the exclude entry says it added it: the
// entry is in the exclude file whether or not the file itself was written.
func TestAFailedWriteReportsWhatItHadDone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root from writing")
	}
	loc := testLocation(t)
	dir := filepath.Dir(loc.File)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	out, err := Write(loc, Facts{Started{alpha}}, at)
	if err == nil {
		t.Fatal("Write into a read-only directory succeeded; want an error")
	}
	want := Outcome{Changed: []string{}, Created: true, ExcludeAdded: true}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("failed Write = %+v, want %+v", out, want)
	}
	if raw, readErr := os.ReadFile(loc.Exclude); readErr != nil || !strings.Contains(string(raw), excludeEntry) {
		t.Fatalf("exclude file = %q, %v; want the entry the Outcome reports", raw, readErr)
	}
}

// A rewrite of an unreadable file that fails does not say the file was
// replaced: the damaged file is still on disk.
func TestAFailedRewriteOfAnUnreadableFileIsNotReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root from writing")
	}
	loc := testLocation(t)
	dir := filepath.Dir(loc.File)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loc.File, []byte("{working_on"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	out, err := Write(loc, Facts{Started{alpha}}, at)
	if err == nil {
		t.Fatal("Write into a read-only directory succeeded; want an error")
	}
	if want := (Outcome{Changed: []string{}}); !reflect.DeepEqual(out, want) {
		t.Fatalf("failed rewrite = %+v, want %+v", out, want)
	}
}

// A file this lit cannot read as its own schema is rewritten from the new
// facts, and the write says so rather than merging into a guess.
func TestAnUnreadableFileIsReplacedAndReported(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":         "{working_on",
		"another schema":   `{"schema_version": 2, "working_on": {"ticket": {"id": "t-old", "title": "Old"}}}`,
		"no schema at all": `{"working_on": {"ticket": {"id": "t-old", "title": "Old"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			loc := testLocation(t)
			if err := os.MkdirAll(filepath.Dir(loc.File), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(loc.File, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			out := write(t, loc, Picked{&bravo})
			if out.Replaced == nil || out.Created {
				t.Fatalf("write over %s = %+v; want Replaced set and Created false", name, out)
			}
			// Nothing of the old file could be read, so no slot can be said to
			// hold what it held before.
			if want := []string{"working_on", "next"}; !reflect.DeepEqual(out.Changed, want) {
				t.Fatalf("write over %s changed %q, want %q", name, out.Changed, want)
			}
			want := File{SchemaVersion: SchemaVersion, Next: &Next{Ticket: &bravo, RecordedAt: at}}
			if got := readFile(t, loc); !reflect.DeepEqual(got, want) {
				t.Fatalf("file = %+v\nwant %+v", got, want)
			}
		})
	}
}

// A write leaves the file and nothing else in its directory: the temporary
// sibling it renames into place is gone.
func TestAWriteLeavesNoTemporaryFile(t *testing.T) {
	loc := testLocation(t)
	write(t, loc, Started{alpha})
	write(t, loc, Picked{&bravo})
	entries, err := os.ReadDir(filepath.Dir(loc.File))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(loc.File) {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("statusline directory holds %v, want only %s", names, filepath.Base(loc.File))
	}
}
