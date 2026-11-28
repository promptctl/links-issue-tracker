// Package selector is the grammar a selection is written in. A selector is a
// conjunction of atoms over a table of fields the caller hands in; the grammar
// knows what kinds a field can have and which operators each kind admits, and
// nothing about which fields exist. Parse turns text into a Selector against a
// Schema, refusing an unknown field or an operator its kind does not admit
// before anything is read, so an engine answering a Selector is only ever
// handed one it can answer. [LAW:parse-dont-validate]
package selector

import (
	"cmp"
	"fmt"
	"slices"
)

// Kind is what a field's values are. It decides the operators the field
// admits, whether a path can step through it, and whether it can be counted.
type Kind int

const (
	// Text is one piece of text; the empty text is no value.
	Text Kind = iota + 1
	// Enum is one member of a closed set.
	Enum
	// Stamp is one instant, compared as time.
	Stamp
	// Search is several texts searched together, by contains only.
	Search
	// Ref is one row of another table, compared by its key and stepped into.
	Ref
	// Rows is a list of rows of another table: compared by their keys,
	// filtered, counted and stepped into.
	Rows
	// Texts is a list of texts.
	Texts
	// Records is a list of records that carry nothing to compare: it is
	// present, absent or counted.
	Records
)

// Op is how an atom tests the values its path reads.
type Op string

const (
	// Present holds when the path reads at least one value.
	Present Op = ""
	// Absent holds when the path reads no value.
	Absent Op = "!"
	// Eq holds when some value read equals some value listed.
	Eq Op = "="
	// Ne holds exactly where Eq does not, so it holds for a path that reads
	// nothing.
	Ne Op = "!="
	// Contains holds when some value read contains some value listed,
	// ignoring case.
	Contains Op = "~"
	Lt       Op = "<"
	Le       Op = "<="
	Gt       Op = ">"
	Ge       Op = ">="
)

// comparisons are the operators that take values, longest spelling first so a
// scanner reading them in this order never takes "<" for the start of "<=".
var comparisons = []Op{Ne, Le, Ge, Eq, Contains, Lt, Gt}

// orderings take exactly one value: "before one of these instants" is a
// question nobody means.
var orderings = []Op{Lt, Le, Gt, Ge}

// rule is what a kind admits, in one row per kind, so the parser and the help
// text read the same answer. [LAW:one-source-of-truth]
type rule struct {
	name string
	ops  []Op
	list bool // takes /count
	says string
}

var rules = map[Kind]rule{
	Text:    {name: "text", ops: []Op{Present, Absent, Eq, Ne, Contains}, says: "one text; an empty text is no value"},
	Enum:    {name: "enum", ops: []Op{Present, Absent, Eq, Ne}, says: "one of the values its field lists"},
	Stamp:   {name: "stamp", ops: []Op{Present, Absent, Eq, Ne, Lt, Le, Gt, Ge}, says: "one instant, compared as time"},
	Search:  {name: "search", ops: []Op{Contains}, says: "several texts, holding when any of them contains the value"},
	Ref:     {name: "ref", ops: []Op{Present, Absent, Eq, Ne}, says: "one row, compared by its key; /field reads its fields and (selector) keeps it only where the selector holds"},
	Rows:    {name: "refs", ops: []Op{Present, Absent, Eq, Ne}, list: true, says: "rows, compared by their keys; /field reads the fields of every row, (selector) keeps the rows it selects"},
	Texts:   {name: "texts", ops: []Op{Present, Absent, Eq, Ne, Contains}, list: true, says: "texts"},
	Records: {name: "records", ops: []Op{Present, Absent}, list: true, says: "records with nothing to compare"},
}

// countRule is what a path ending in /count admits: a number has no presence
// and contains nothing.
var countRule = rule{name: "count", ops: []Op{Eq, Ne, Lt, Le, Gt, Ge}, says: "after /count on a list: how many distinct values it reads, a whole number"}

func (k Kind) rule() rule { return rules[k] }

// Field is one readable field of a table. It is built by the constructor for
// its kind, so a field whose values are rows always names their table and an
// enum always lists its members.
type Field struct {
	name    string
	kind    Kind
	doc     string
	parse   func(string) (string, error)
	members []string
	of      *Table
}

func (f *Field) Name() string { return f.name }
func (f *Field) Kind() Kind   { return f.kind }
func (f *Field) Doc() string  { return f.doc }

// Of is the table a Ref or Rows field's rows belong to.
func (f *Field) Of() *Table { return f.of }

// canonical is the form an = or != value is compared in, which the field's
// own domain parser decides, so a value typed at any surface is compared in
// the one spelling the store holds. [LAW:single-enforcer]
func (f *Field) canonical(value string) (string, error) {
	switch {
	case f.of != nil:
		return f.of.key.canonical(value)
	case f.parse != nil:
		return f.parse(value)
	default:
		return value, nil
	}
}

// TextField is a field holding one text, compared in the form parse gives it;
// a nil parse compares the text as typed.
func TextField(name, doc string, parse func(string) (string, error)) *Field {
	return &Field{name: name, kind: Text, doc: doc, parse: parse}
}

// EnumField is a field holding one of members, a value parsed by the domain's
// own parser for that vocabulary.
func EnumField(name, doc string, members []string, parse func(string) (string, error)) *Field {
	return &Field{name: name, kind: Enum, doc: doc, members: members, parse: parse}
}

// StampField is a field holding one instant.
func StampField(name, doc string) *Field { return &Field{name: name, kind: Stamp, doc: doc} }

// SearchField is a field holding several texts searched together.
func SearchField(name, doc string) *Field { return &Field{name: name, kind: Search, doc: doc} }

// RefField is a field holding one row of of.
func RefField(name, doc string, of *Table) *Field {
	return &Field{name: name, kind: Ref, doc: doc, of: of}
}

// RowsField is a field holding a list of rows of of.
func RowsField(name, doc string, of *Table) *Field {
	return &Field{name: name, kind: Rows, doc: doc, of: of}
}

// TextsField is a field holding a list of texts, each compared in the form
// parse gives it.
func TextsField(name, doc string, parse func(string) (string, error)) *Field {
	return &Field{name: name, kind: Texts, doc: doc, parse: parse}
}

// RecordsField is a field holding a list of records.
func RecordsField(name, doc string) *Field { return &Field{name: name, kind: Records, doc: doc} }

// Table is a set of fields a path can read, one of which is the key its rows
// are named by.
type Table struct {
	name   string
	key    *Field
	fields []*Field
}

// NewTable is a table with no fields yet. A table's fields can name the table
// itself (an issue's parent is an issue), so the table exists before them and
// Define gives it its fields once they do.
func NewTable(name string) *Table { return &Table{name: name} }

// Define gives t its key and fields, once.
func (t *Table) Define(key *Field, fields ...*Field) {
	if t.fields != nil {
		panic(fmt.Sprintf("selector: table %s is already defined", t.name))
	}
	if key != nil && !slices.Contains(fields, key) {
		panic(fmt.Sprintf("selector: table %s's key %s is not one of its fields", t.name, key.name))
	}
	t.key, t.fields = key, fields
}

func (t *Table) Name() string     { return t.name }
func (t *Table) Fields() []*Field { return t.fields }
func (t *Table) field(name string) *Field {
	for _, f := range t.fields {
		if f.name == name {
			return f
		}
	}
	return nil
}

// Schema is what a selector is parsed against: the table its atoms read, and
// the table a path written with a leading / starts at instead.
type Schema struct {
	Root   *Table
	Anchor *Table
}

// Selector is a conjunction: it holds for a row when every atom does. The
// zero Selector has no atoms and holds for every row.
type Selector struct {
	atoms []Atom
}

func (s Selector) Atoms() []Atom { return s.atoms }

// And is the conjunction of every atom of parts, so adding a part can only
// narrow what is selected.
func And(parts ...Selector) Selector {
	var atoms []Atom
	for _, part := range parts {
		atoms = append(atoms, part.atoms...)
	}
	return Selector{atoms: atoms}
}

// Starts reports whether an atom of s reads f directly from the row, rather
// than through another field or from the anchor.
func (s Selector) Starts(f *Field) bool {
	return slices.ContainsFunc(s.atoms, func(a Atom) bool {
		return !a.path.anchored && a.path.steps[0].field == f
	})
}

// Reads reports whether s reads f anywhere: at any step of any atom's path,
// or in any filter on one.
func (s Selector) Reads(f *Field) bool {
	return slices.ContainsFunc(s.atoms, func(a Atom) bool {
		return slices.ContainsFunc(a.path.steps, func(step Step) bool {
			return step.field == f || step.where.Reads(f)
		})
	})
}

// Atom tests the values a path reads.
type Atom struct {
	path Path
	test Test
}

func (a Atom) Path() Path { return a.path }
func (a Atom) Test() Test { return a.test }

// Path is the fields an atom reads, each step from the rows the step before it
// reached: from the row itself, or from the anchor when the path is anchored.
type Path struct {
	anchored bool
	steps    []Step
	count    bool
}

func (p Path) Anchored() bool { return p.anchored }
func (p Path) Steps() []Step  { return p.steps }

// Counts reports whether the atom tests how many distinct values the path
// reads rather than the values themselves.
func (p Path) Counts() bool { return p.count }

// Last is the field the path ends at, whose values the atom tests.
func (p Path) Last() *Field { return p.steps[len(p.steps)-1].field }

// Step reads one field. A step whose values are rows keeps only the rows its
// Where selects; the zero Where keeps every row.
type Step struct {
	field *Field
	where Selector
}

func (s Step) Field() *Field   { return s.field }
func (s Step) Where() Selector { return s.where }

// Test is what an atom asks of its values: Presence, Match or Count.
type Test interface{ test() }

// Presence holds when the path reads a value (Present) or none (Absent).
type Presence struct{ Op Op }

// Match compares the values read with Values, each already in the canonical
// form the field compares in: a stamp is its stamp text, which orders as its
// instant does. An ordering Op has exactly one value.
type Match struct {
	Op     Op
	Values []string
}

// Count compares the number of distinct values read with Values. An ordering
// Op has exactly one value.
type Count struct {
	Op     Op
	Values []int
}

// Holds is whether a path that reads n distinct values satisfies the test.
func (c Count) Holds(n int) bool {
	if c.Op == Eq || c.Op == Ne {
		return slices.Contains(c.Values, n) == (c.Op == Eq)
	}
	return Ordered(c.Op, n, c.Values[0])
}

// Ordered is whether v stands to bound as the ordering operator op says.
func Ordered[T cmp.Ordered](op Op, v, bound T) bool {
	switch op {
	case Lt:
		return v < bound
	case Le:
		return v <= bound
	case Gt:
		return v > bound
	case Ge:
		return v >= bound
	default:
		panic(fmt.Sprintf("selector: %v is not an ordering", op))
	}
}

func (Presence) test() {}
func (Match) test()    {}
func (Count) test()    {}
