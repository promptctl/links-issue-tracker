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
	progress     = RollupProgress{CompletedCount: 1, TotalCount: 4}
	bravoLane    = RollupProgress{CompletedCount: 0, TotalCount: 2}
	review       = "review"
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
	working := func(ticket Ticket, lane RollupProgress) *WorkingOn {
		return &WorkingOn{Ticket: ticket, RollupProgress: lane, RecordedAt: at}
	}
	inPhase := func(ticket Ticket, lane RollupProgress, phase string) *WorkingOn {
		return &WorkingOn{Ticket: ticket, RollupProgress: lane, Phase: &phase, RecordedAt: at}
	}
	next := func(ticket *Ticket) *Next { return &Next{Ticket: ticket, RecordedAt: at} }
	cases := []struct {
		name   string
		writes [][]Fact
		want   File
	}{
		{
			name:   "a start records what the checkout is working on and its lane's count",
			writes: [][]Fact{{Started{alpha, progress}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha, progress)},
		},
		{
			name:   "a later write keeps the slots it says nothing about",
			writes: [][]Fact{{Started{alpha, progress}}, {Picked{&bravo}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha, progress), Next: next(&bravo)},
		},
		{
			name:   "a pick of nothing is recorded, not left blank",
			writes: [][]Fact{{Picked{&alpha}}, {Picked{nil}}},
			want:   File{SchemaVersion: SchemaVersion, Next: next(nil)},
		},
		{
			name:   "a second start replaces the first",
			writes: [][]Fact{{Started{alpha, progress}}, {Started{bravo, bravoLane}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo, bravoLane)},
		},
		{
			name:   "invalidating a ticket clears every slot naming it",
			writes: [][]Fact{{Started{alpha, progress}, Picked{&alpha}}, {Invalidated{alpha.ID}}},
			want:   File{SchemaVersion: SchemaVersion},
		},
		{
			name:   "invalidating another ticket clears nothing",
			writes: [][]Fact{{Started{alpha, progress}, Picked{&bravo}}, {Invalidated{"t-other"}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha, progress), Next: next(&bravo)},
		},
		{
			name:   "invalidating the pick leaves the work in flight",
			writes: [][]Fact{{Started{alpha, progress}, Picked{&bravo}}, {Invalidated{bravo.ID}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha, progress)},
		},
		{
			name:   "a retitle renames every slot naming the ticket",
			writes: [][]Fact{{Started{alpha, progress}, Picked{&alpha}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alphaRenamed, progress), Next: next(&alphaRenamed)},
		},
		{
			name:   "retitling another ticket renames nothing",
			writes: [][]Fact{{Started{alpha, progress}, Picked{&bravo}}, {Retitled{Ticket{ID: "t-other", Title: "Other"}}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha, progress), Next: next(&bravo)},
		},
		{
			name:   "retitling leaves a recorded pick of nothing alone",
			writes: [][]Fact{{Picked{nil}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, Next: next(nil)},
		},
		{
			name:   "a retitle keeps the rollup count",
			writes: [][]Fact{{Started{alpha, progress}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alphaRenamed, progress)},
		},
		{
			name:   "a new start shows its own lane's rollup count",
			writes: [][]Fact{{Started{alpha, progress}}, {Started{bravo, bravoLane}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo, bravoLane)},
		},
		{
			name:   "a phase shows on the work it names",
			writes: [][]Fact{{Started{alpha, progress}, PhaseRead{alpha.ID, "plan"}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: inPhase(alpha, progress, "plan")},
		},
		{
			name:   "a later phase replaces the one recorded",
			writes: [][]Fact{{Started{alpha, progress}, PhaseRead{alpha.ID, "plan"}}, {PhaseRead{alpha.ID, "impl"}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: inPhase(alpha, progress, "impl")},
		},
		{
			name:   "a phase for another ticket shows nowhere",
			writes: [][]Fact{{Started{alpha, progress}, Picked{&bravo}, PhaseRead{bravo.ID, "impl"}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(alpha, progress), Next: next(&bravo)},
		},
		{
			name:   "a phase with no work recorded shows nowhere",
			writes: [][]Fact{{PhaseRead{alpha.ID, "impl"}}},
			want:   File{SchemaVersion: SchemaVersion},
		},
		{
			name:   "a retitle keeps the phase",
			writes: [][]Fact{{Started{alpha, progress}, PhaseRead{alpha.ID, "review"}}, {Retitled{alphaRenamed}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: inPhase(alphaRenamed, progress, "review")},
		},
		{
			name:   "a new start drops the last ticket's phase",
			writes: [][]Fact{{Started{alpha, progress}, PhaseRead{alpha.ID, "review"}}, {Started{bravo, bravoLane}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo, bravoLane)},
		},
		{
			name:   "invalidating the work clears its phase with it",
			writes: [][]Fact{{Started{alpha, progress}, PhaseRead{alpha.ID, "merge"}}, {Invalidated{alpha.ID}}},
			want:   File{SchemaVersion: SchemaVersion},
		},
		{
			name:   "facts apply in the order the command learned them",
			writes: [][]Fact{{Started{alpha, progress}, Invalidated{alpha.ID}, Started{bravo, bravoLane}}},
			want:   File{SchemaVersion: SchemaVersion, WorkingOn: working(bravo, bravoLane)},
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
	write(t, loc, Started{alpha, progress}, Picked{&alpha})
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
	write(t, loc, Started{alpha, progress}, PhaseRead{alpha.ID, "impl"}, Picked{nil})
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
			"ticket":          map[string]any{"id": "t-alpha", "title": "Alpha"},
			"rollup_progress": map[string]any{"rollup_completed_count": float64(1), "rollup_total_count": float64(4)},
			"phase":           "impl",
			"recorded_at":     "2026-10-15T09:30:00Z",
		},
		"next": map[string]any{"ticket": nil, "recorded_at": "2026-10-15T09:30:00Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("file = %s\nwant %v", raw, want)
	}
}

// lit only ever adds fields to the file, so a plugin written for any lit reads
// a file from any other. This list is every field lit has written at
// schema_version 1, and it only grows: a field is never edited or removed
// here, and a field lit starts writing joins it, so the promise holds for that
// field too. It is not the set every version-1 file carries: a lit older than
// a field never wrote it.
// [LAW:one-source-of-truth] exception: the contract is a second copy of the
// shape on purpose, so a rename the shape test would absorb still fails here.
var schemaV1Fields = []string{
	"schema_version",
	"working_on",
	"working_on.ticket",
	"working_on.ticket.id",
	"working_on.ticket.title",
	"working_on.rollup_progress",
	"working_on.rollup_progress.rollup_completed_count",
	"working_on.rollup_progress.rollup_total_count",
	"working_on.phase",
	"working_on.recorded_at",
	"next",
	"next.ticket",
	"next.ticket.id",
	"next.ticket.title",
	"next.recorded_at",
}

// Each write leaves a different slot null, and one writes a zero count, since a
// key dropped only while its value is null or zero would turn "recorded as
// nothing" into "not recorded".
func TestEveryVersion1FieldIsStillWritten(t *testing.T) {
	contract := map[string]bool{}
	for _, field := range schemaV1Fields {
		contract[field] = true
	}
	for _, facts := range [][]Fact{
		{Started{alpha, progress}, PhaseRead{alpha.ID, "impl"}, Picked{&bravo}},
		{Started{alpha, progress}, Picked{nil}},
		{Picked{&bravo}},
		{Started{bravo, bravoLane}},
	} {
		loc := testLocation(t)
		write(t, loc, facts...)
		raw, err := os.ReadFile(loc.File)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		// objects maps each object the file holds to its keys; "" is the root.
		// values holds every field written, so a parent that stopped being an
		// object is told apart from one that is null.
		objects := map[string]map[string]any{}
		values := map[string]any{}
		var walk func(path string, obj map[string]any)
		walk = func(path string, obj map[string]any) {
			objects[path] = obj
			for k, v := range obj {
				field := strings.TrimPrefix(path+"."+k, ".")
				values[field] = v
				if !contract[field] {
					t.Errorf("%s is written but not in schemaV1Fields; a field lit writes joins that list, so it is never removed either\n%s", field, raw)
				}
				if child, ok := v.(map[string]any); ok {
					walk(field, child)
				}
			}
		}
		walk("", doc)
		for _, field := range schemaV1Fields {
			parent, key := "", field
			if i := strings.LastIndex(field, "."); i >= 0 {
				parent, key = field[:i], field[i+1:]
			}
			if v, written := values[parent]; parent != "" && (!written || v == nil) {
				continue // a null or absent parent holds nothing; an absent one fails as its own field
			}
			if _, ok := objects[parent][key]; !ok {
				t.Errorf("%s is missing from the file; lit never removes or renames a field of schema_version 1 (docs/statusline.md, schema_version)\n%s", field, raw)
			}
		}
		if v := doc["schema_version"]; v != float64(1) {
			t.Errorf("schema_version = %v, want 1: it never changes", v)
		}
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
	first := write(t, loc, Started{alpha, progress})
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
		recreated := write(t, loc, Started{alpha, progress})
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
	write(t, loc, Started{alpha, progress})
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
		{[]Fact{Started{alpha, progress}}, []string{"working_on"}},
		{[]Fact{Picked{&bravo}}, []string{"next"}},
		{[]Fact{Started{alpha, progress}}, []string{"working_on"}},
		{[]Fact{Retitled{Ticket{ID: "t-other", Title: "Other"}}}, []string{}},
		{[]Fact{Invalidated{"t-other"}}, []string{}},
		{[]Fact{Retitled{alphaRenamed}}, []string{"working_on"}},
		{[]Fact{Invalidated{bravo.ID}}, []string{"next"}},
		{[]Fact{Started{bravo, bravoLane}, Picked{&alpha}}, []string{"working_on", "next"}},
		{[]Fact{PhaseRead{bravo.ID, "impl"}}, []string{"working_on"}},
		{[]Fact{PhaseRead{bravo.ID, "impl"}}, []string{}},
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

// Each write reports the rollup progress the working_on it wrote shows, and
// nothing when that record is null.
func TestAWriteReportsTheRollupProgressItsWorkingOnShows(t *testing.T) {
	loc := testLocation(t)
	for _, step := range []struct {
		facts []Fact
		want  *RollupProgress
	}{
		{[]Fact{Picked{&bravo}}, nil},
		{[]Fact{Started{alpha, progress}}, &progress},
		{[]Fact{Retitled{alphaRenamed}}, &progress},
		{[]Fact{Invalidated{alpha.ID}}, nil},
	} {
		out := write(t, loc, step.facts...)
		if !reflect.DeepEqual(out.RollupProgress, step.want) {
			t.Fatalf("Write(%v).RollupProgress = %v, want %v", step.facts, out.RollupProgress, step.want)
		}
	}
}

// A working_on no command read a phase for encodes it as null, not as a
// missing field: missing is what an older lit's file says.
func TestAnUnreadPhaseEncodesAsNull(t *testing.T) {
	loc := testLocation(t)
	write(t, loc, Started{alpha, progress})
	raw, err := os.ReadFile(loc.File)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		WorkingOn map[string]any `json:"working_on"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if phase, ok := got.WorkingOn["phase"]; !ok || phase != nil {
		t.Fatalf("working_on.phase = %v (present %t) in %s; want null", phase, ok, raw)
	}
}

// Each write reports the phase the working_on it wrote shows, whichever command
// read it, and nothing when that record shows none or is null.
func TestAWriteReportsThePhaseItsWorkingOnShows(t *testing.T) {
	loc := testLocation(t)
	for _, step := range []struct {
		facts []Fact
		want  *string
	}{
		{[]Fact{Picked{&bravo}}, nil},
		{[]Fact{Started{alpha, progress}}, nil},
		{[]Fact{PhaseRead{alpha.ID, review}}, &review},
		{[]Fact{Retitled{alphaRenamed}}, &review},
		{[]Fact{Invalidated{alpha.ID}}, nil},
	} {
		out := write(t, loc, step.facts...)
		if !reflect.DeepEqual(out.Phase, step.want) {
			t.Fatalf("Write(%v).Phase = %v, want %v", step.facts, out.Phase, step.want)
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
	out, err := Write(loc, Facts{Started{alpha, progress}}, at)
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
	out, err := Write(loc, Facts{Started{alpha, progress}}, at)
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

// A working_on an older lit wrote with no count is the one record dropped: the
// file's next is still read and kept, and the write says what it dropped.
func TestAnUncountedWorkingOnIsDroppedAndNextKept(t *testing.T) {
	const next = `"next": {"ticket": {"id": "t-next", "title": "Next"}, "recorded_at": "2026-10-15T09:30:00Z"}`
	for name, workingOn := range map[string]string{
		// lit 0.19 to 0.21 wrote null for a lane the recording command did not
		// count, and a lit before them wrote no key.
		"a null rollup_progress": `{"ticket": {"id": "t-old", "title": "Old"}, "rollup_progress": null, "phase": null, "recorded_at": "2026-10-15T09:30:00Z"}`,
		"no rollup_progress":     `{"ticket": {"id": "t-old", "title": "Old"}, "phase": null, "recorded_at": "2026-10-15T09:30:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			loc := testLocation(t)
			if err := os.MkdirAll(filepath.Dir(loc.File), 0o755); err != nil {
				t.Fatal(err)
			}
			content := `{"schema_version": 1, "working_on": ` + workingOn + `, ` + next + `}`
			if err := os.WriteFile(loc.File, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			out := write(t, loc, Retitled{Ticket{ID: "t-other", Title: "Other"}})
			if out.Replaced == nil || !strings.Contains(out.Replaced.Error(), "working_on has no rollup_progress") {
				t.Fatalf("write over %s: Replaced = %v; want the dropped working_on named", name, out.Replaced)
			}
			if want := []string{"working_on"}; !reflect.DeepEqual(out.Changed, want) {
				t.Fatalf("write over %s changed %q, want %q", name, out.Changed, want)
			}
			want := File{SchemaVersion: SchemaVersion, Next: &Next{Ticket: &Ticket{ID: "t-next", Title: "Next"}, RecordedAt: time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)}}
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
	write(t, loc, Started{alpha, progress})
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
