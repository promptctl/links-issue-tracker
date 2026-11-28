package selector

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

var now = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

// testSchema builds a small table of every kind, fresh for each test, so a
// test that adds a field changes nothing another test reads.
func testSchema() (Schema, map[string]*Field) {
	things := NewTable("thing")
	world := NewTable("world")
	upper := func(s string) (string, error) {
		if s != strings.TrimSpace(s) {
			return "", errors.New("no spaces around a name")
		}
		return strings.ToUpper(s), nil
	}
	color := func(s string) (string, error) {
		if s != "red" && s != "blue" {
			return "", errors.New("red or blue")
		}
		return s, nil
	}
	fields := map[string]*Field{
		"id":     TextField("id", "the key", upper),
		"name":   TextField("name", "a name", nil),
		"color":  EnumField("color", "a color.", []string{"red", "blue"}, color),
		"at":     StampField("at", "an instant"),
		"words":  SearchField("words", "every text"),
		"owner":  RefField("owner", "another thing", things),
		"parts":  RowsField("parts", "things", things),
		"tags":   TextsField("tags", "tags", func(s string) (string, error) { return strings.ToLower(s), nil }),
		"notes":  RecordsField("notes", "notes"),
		"things": RowsField("things", "every thing", things),
	}
	things.Define(fields["id"], fields["id"], fields["name"], fields["color"], fields["at"], fields["words"], fields["owner"], fields["parts"], fields["tags"], fields["notes"])
	world.Define(nil, fields["things"])
	return Schema{Root: things, Anchor: world}, fields
}

func TestParseReadsEveryAtomForm(t *testing.T) {
	schema, f := testSchema()
	one := func(fields ...*Field) []Step {
		steps := make([]Step, 0, len(fields))
		for _, field := range fields {
			steps = append(steps, Step{field: field})
		}
		return steps
	}
	cases := []struct {
		text string
		want Atom
	}{
		{"name", Atom{path: Path{steps: one(f["name"])}, test: Presence{Op: Present}}},
		{"!name", Atom{path: Path{steps: one(f["name"])}, test: Presence{Op: Absent}}},
		{"color=red,blue", Atom{path: Path{steps: one(f["color"])}, test: Match{Op: Eq, Values: []string{"red", "blue"}}}},
		{"color!=red", Atom{path: Path{steps: one(f["color"])}, test: Match{Op: Ne, Values: []string{"red"}}}},
		{"name~FoO", Atom{path: Path{steps: one(f["name"])}, test: Match{Op: Contains, Values: []string{"foo"}}}},
		{`name="a b, (c) \"d\" \\"`, Atom{path: Path{steps: one(f["name"])}, test: Match{Op: Eq, Values: []string{`a b, (c) "d" \`}}}},
		{"tags=Wip", Atom{path: Path{steps: one(f["tags"])}, test: Match{Op: Eq, Values: []string{"wip"}}}},
		{"owner=abc", Atom{path: Path{steps: one(f["owner"])}, test: Match{Op: Eq, Values: []string{"ABC"}}}},
		{"owner/color=red", Atom{path: Path{steps: one(f["owner"], f["color"])}, test: Match{Op: Eq, Values: []string{"red"}}}},
		{"parts/count>=2", Atom{path: Path{steps: one(f["parts"]), count: true}, test: Count{Op: Ge, Values: []int{2}}}},
		{"at<now-6h", Atom{path: Path{steps: one(f["at"])}, test: Match{Op: Lt, Values: []string{model.StampText(now.Add(-6 * time.Hour))}}}},
		{"at>=now+2w", Atom{path: Path{steps: one(f["at"])}, test: Match{Op: Ge, Values: []string{model.StampText(now.Add(14 * 24 * time.Hour))}}}},
		{"at=2026-01-02T15:04:05+01:00", Atom{path: Path{steps: one(f["at"])}, test: Match{Op: Eq, Values: []string{model.StampText(time.Date(2026, 1, 2, 14, 4, 5, 0, time.UTC))}}}},
		{"/things/count=0", Atom{path: Path{anchored: true, steps: one(f["things"]), count: true}, test: Count{Op: Eq, Values: []int{0}}}},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, err := Parse(schema, tc.text, now)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.text, err)
			}
			if len(got.atoms) != 1 || !reflect.DeepEqual(got.atoms[0], tc.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tc.text, got.atoms, tc.want)
			}
		})
	}
}

func TestParseReadsFiltersAndConjunctions(t *testing.T) {
	schema, f := testSchema()
	got, err := Parse(schema, "  color=red   parts(color!=blue !notes)/count=0 /things(name~x)/owner ", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.atoms) != 3 {
		t.Fatalf("Parse() read %d atoms, want 3: %#v", len(got.atoms), got.atoms)
	}
	filter := got.atoms[1].path.steps[0].where.atoms
	if len(filter) != 2 || filter[0].path.Last() != f["color"] || filter[1].test != (Presence{Op: Absent}) {
		t.Fatalf("the filter on parts read %#v, want color!=blue and !notes", filter)
	}
	third := got.atoms[2].path
	if !third.anchored || third.steps[0].field != f["things"] || third.Last() != f["owner"] || len(third.steps[0].where.atoms) != 1 {
		t.Fatalf("the anchored atom read %#v, want /things(name~x)/owner", third)
	}
}

func TestParseRefusesWhatItCannotRead(t *testing.T) {
	schema, _ := testSchema()
	cases := []struct {
		text string
		says string
	}{
		{"blocked", `unknown field "blocked"`},
		{"owner/blocked=x", `unknown field "blocked"`},
		{"color:red", `unexpected ':' after color`},
		{"lunch", `unknown field "lunch"`},
		{"updated>=2026-01-01T00:00:00Z", `unknown field "updated"`},
		{"!color=red", "takes no operator"},
		{"color~re", "takes path !path = !="},
		{"words=x", "takes ~"},
		{"notes=x", "takes path !path"},
		{"color=green", "red or blue"},
		{"color=", "empty value; !color is the path that reads nothing"},
		{"color=red,", "empty value"},
		{"at<now,now-1h", "takes one value"},
		{"at<yesterday", "a stamp is now"},
		{"at<now-6y", "a unit of m, h, d or w"},
		{"at<now-", "a unit of m, h, d or w"},
		{"at<now+", "a unit of m, h, d or w"},
		{"at<nowx", "a unit of m, h, d or w"},
		{"at<now-h", "a unit of m, h, d or w"},
		{"at<now-6hh", "a unit of m, h, d or w"},
		{"at<now-99999999999999w", "a unit of m, h, d or w"},
		{"owner/count=1", "count counts a list"},
		{"parts/count", "takes = != < <= > >="},
		{"parts/count=-1", "a whole number"},
		{"parts/count/name", "count ends a path"},
		{"name(color=red)", "only a field whose values are rows takes a selector"},
		{"name/color", "name has no fields to read"},
		{"parts(color=red", "never closed"},
		{"color=red)", "closes nothing"},
		{`name="abc`, "never closed"},
		{`name="a\nb"`, "is not an escape"},
		{"name=a(b", "quote a value"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			_, err := Parse(schema, tc.text, now)
			var invalid model.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Parse(%q) error = %v, want a model.ValidationError", tc.text, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Parse(%q) error = %q, want it to say %q", tc.text, err, tc.says)
			}
		})
	}
}

func TestUnknownFieldNamesTheFieldsThatExist(t *testing.T) {
	schema, _ := testSchema()
	_, err := Parse(schema, "blocked", now)
	if err == nil || !strings.Contains(err.Error(), "id, name, color, at, words, owner, parts, tags, notes") {
		t.Fatalf("Parse(blocked) error = %v, want it to list the table's fields", err)
	}
}

func TestParseAtomReadsAsTheSameAtomInText(t *testing.T) {
	schema, _ := testSchema()
	cases := []struct {
		path   string
		op     Op
		values []string
		text   string
	}{
		{"color", Eq, []string{"red", "blue"}, "color=red,blue"},
		{"tags", Eq, []string{"Needs-Design"}, "tags=Needs-Design"},
		{"notes", Absent, nil, "!notes"},
		{"at", Ge, []string{"now-1d"}, "at>=now-1d"},
		{"parts", Eq, []string{"a b"}, `parts="a b"`},
		{"words", Contains, []string{"Two Words"}, `words~"Two Words"`},
	}
	for _, tc := range cases {
		atom, err := ParseAtom(schema, now, tc.path, tc.op, tc.values...)
		if err != nil {
			t.Fatalf("ParseAtom(%s %q %v) error = %v", tc.path, tc.op, tc.values, err)
		}
		text, err := Parse(schema, tc.text, now)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", tc.text, err)
		}
		if !reflect.DeepEqual(atom, text) {
			t.Errorf("ParseAtom(%s %q %v) = %#v, want what Parse(%q) reads, %#v", tc.path, tc.op, tc.values, atom, tc.text, text)
		}
	}
	if _, err := ParseAtom(schema, now, "color", Eq, "green"); err == nil {
		t.Error("ParseAtom(color = green) accepted a value the field's parser refuses")
	}
}

// Is is the atom the same values typed read as, and a value the grammar
// refuses is a program's mistake it panics on, never an atom that selects
// nothing, or everything, in silence.
func TestIsReadsAsTheSameAtomInText(t *testing.T) {
	schema, f := testSchema()
	text, err := Parse(schema, "id=a,b", now)
	if err != nil {
		t.Fatalf("Parse(id=a,b) error = %v", err)
	}
	if got := Is(f["id"], "a", "b"); !reflect.DeepEqual(got, text) {
		t.Errorf("Is(id, a, b) = %#v, want what Parse(id=a,b) reads, %#v", got, text)
	}
	for _, tc := range []struct {
		name   string
		field  string
		values []string
	}{
		{"no value", "id", nil},
		{"an empty value", "id", []string{"a", ""}},
		{"a value the field refuses", "color", []string{"green"}},
		{"a stamp, which reads now", "at", []string{"now"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Is(%s, %q) built an atom", tc.field, tc.values)
				}
			}()
			Is(f[tc.field], tc.values...)
		})
	}
}

func TestStartsReadsOnlyTheRowsOwnField(t *testing.T) {
	schema, f := testSchema()
	sel, err := Parse(schema, "owner/color=red /things(color=red)/count=1 name", now)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Starts(f["color"]) {
		t.Error("Starts(color) is true for a selector that reads color only through owner and the workspace")
	}
	if !sel.Starts(f["owner"]) || !sel.Starts(f["name"]) {
		t.Error("Starts is false for a field an atom reads from the row")
	}
}

func TestReadsSeesAFieldWhereverAPathReadsIt(t *testing.T) {
	schema, f := testSchema()
	for _, text := range []string{"color=red", "owner/color=red", "/things(color=red)/count=1", "owner(owner(color=blue))"} {
		sel, err := Parse(schema, text, now)
		if err != nil {
			t.Fatal(err)
		}
		if !sel.Reads(f["color"]) {
			t.Errorf("%q: Reads(color) = false, want true", text)
		}
	}
	sel, err := Parse(schema, "owner/name=x /things/count=1", now)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Reads(f["color"]) {
		t.Error("Reads(color) is true for a selector that never names color")
	}
}

func TestDescribeListsWhatTheTableHolds(t *testing.T) {
	schema, _ := testSchema()
	before := Describe(schema, []Example{{Text: "color=red", Says: "the red things"}})
	for _, want := range []string{"\n  name             text ", "\n  color            enum     a color. One of red, blue.", "\n  things           refs ", "color=red  the red things", "\n  stamp ", "\n  count "} {
		if !strings.Contains(before, want) {
			t.Errorf("Describe() lacks %q:\n%s", want, before)
		}
	}

	grown, _ := testSchema()
	shape := TextField("shape", "a shape the table grew", nil)
	grown.Root.fields = append(grown.Root.fields, shape)
	if !strings.Contains(Describe(grown, nil), "\n  shape            text     a shape the table grew") {
		t.Error("Describe() does not list a field added to the table")
	}
	if _, err := Parse(grown, "shape~oval", now); err != nil {
		t.Errorf("Parse() refuses a field added to the table: %v", err)
	}
}

// Parse answers any text with a selector or a refusal, never a panic: a
// selector is typed by people and agents, and a crash is no answer.
func FuzzParseNeverPanics(f *testing.F) {
	for _, seed := range []string{"", "name", "!name", "color=red,blue", "at<now-6h", "at<now-", "owner/color=red", "parts(color!=blue !notes)/count=0", "/things(name~x)/owner", `name="a \"b\""`, "parts(", ")", "!", "=", "name=,", "parts/count>="} {
		f.Add(seed)
	}
	schema, _ := testSchema()
	f.Fuzz(func(t *testing.T, text string) {
		_, _ = Parse(schema, text, now)
	})
}
