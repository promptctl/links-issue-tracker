package selector

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Parse reads text as a selector against schema:
//
//	selector = atom atom ...
//	atom     = path | !path | path OP value[,value...]
//	path     = [/] segment/segment/...
//	segment  = name | name(selector)
//
// now is the instant a stamp written from now (now-6h) is read against. It is
// read here, once, so a Selector holds only instants and answers the same way
// whenever and wherever it is read. [LAW:effects-at-boundaries]
//
// A refusal is a model.ValidationError: it rejects the text itself.
func Parse(schema Schema, text string, now time.Time) (Selector, error) {
	p := &parser{src: text, schema: schema, now: now}
	sel, err := p.selector(schema.Root, false)
	if err != nil {
		return Selector{}, model.ValidationError{Message: fmt.Sprintf("selector %q: %v", text, err)}
	}
	return sel, nil
}

// ParseAtom is one atom given as its parts: the path's text, the operator, and
// the values as typed. It is the half of Parse that reads one atom, so a value
// that arrives some other way than in a selector's text is refused and
// canonicalized exactly as the same value written in one would be.
// [LAW:single-enforcer]
func ParseAtom(schema Schema, now time.Time, path string, op Op, values ...string) (Selector, error) {
	p := &parser{src: path, schema: schema, now: now}
	parsed, err := p.path(schema.Root)
	if err == nil && !p.eof() {
		err = fmt.Errorf("unexpected %q after %s", p.rest(), path)
	}
	if err != nil {
		return Selector{}, model.ValidationError{Message: fmt.Sprintf("selector path %q: %v", path, err)}
	}
	atom, err := p.finish(parsed, op, values)
	if err != nil {
		return Selector{}, model.ValidationError{Message: fmt.Sprintf("selector %s%s%s: %v", path, op, strings.Join(values, ","), err)}
	}
	return Selector{atoms: []Atom{atom}}, nil
}

// Is is the atom f=values that ParseAtom reads, for a program building a
// selector from values it states: refused and canonicalized as the same values
// typed would be, where a refusal is the program's mistake and panics. A stamp
// is read against the command's now, which a program has only through
// ParseAtom. [LAW:single-enforcer] [LAW:no-silent-failure]
func Is(f *Field, values ...string) Selector {
	if f.kind == Stamp {
		panic(fmt.Sprintf("selector: %s is a stamp, read against the command's now through ParseAtom", f.name))
	}
	atom, err := (&parser{}).finish(Path{steps: []Step{{field: f}}}, Eq, values)
	if err != nil {
		panic(fmt.Sprintf("selector: %s=%s: %v", f.name, strings.Join(values, ","), err))
	}
	return Selector{atoms: []Atom{atom}}
}

type parser struct {
	src    string
	pos    int
	schema Schema
	now    time.Time
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() rune {
	r, _ := utf8.DecodeRuneInString(p.src[p.pos:])
	return r
}

func (p *parser) rest() string { return p.src[p.pos:] }

// next reads the rune at pos and moves past it by the width it was read at,
// which for a byte that is not UTF-8 is one byte rather than the three of the
// replacement rune it reads as. Every move through the text is this one, so
// the position never leaves the text. [LAW:single-enforcer]
func (p *parser) next() rune {
	r, width := utf8.DecodeRuneInString(p.src[p.pos:])
	p.pos += width
	return r
}

func (p *parser) consume(r rune) bool {
	if !p.eof() && p.peek() == r {
		p.next()
		return true
	}
	return false
}

func (p *parser) skipSpace() {
	for !p.eof() && unicode.IsSpace(p.peek()) {
		p.next()
	}
}

// boundary is where an atom may end: the end of the text, a space before the
// next atom, or the ) closing a filter.
func (p *parser) boundary() bool {
	return p.eof() || unicode.IsSpace(p.peek()) || p.peek() == ')'
}

func (p *parser) selector(t *Table, nested bool) (Selector, error) {
	var atoms []Atom
	for {
		p.skipSpace()
		switch {
		case p.eof() && nested:
			return Selector{}, fmt.Errorf("a ( is never closed")
		case p.eof():
			return Selector{atoms: atoms}, nil
		case p.peek() == ')' && nested:
			return Selector{atoms: atoms}, nil
		case p.peek() == ')':
			return Selector{}, fmt.Errorf("a ) closes nothing")
		}
		atom, err := p.atom(t)
		if err != nil {
			return Selector{}, err
		}
		atoms = append(atoms, atom)
	}
}

func (p *parser) atom(t *Table) (Atom, error) {
	absent := p.consume('!')
	path, err := p.path(t)
	if err != nil {
		return Atom{}, err
	}
	if absent {
		if !p.boundary() {
			return Atom{}, fmt.Errorf("!%s takes no operator: it holds where %s reads no value", path.text(), path.text())
		}
		return p.finish(path, Absent, nil)
	}
	if p.boundary() {
		return p.finish(path, Present, nil)
	}
	op, ok := p.op()
	if !ok {
		return Atom{}, fmt.Errorf("unexpected %q after %s; an atom is path, !path or path OP value, with OP one of %s", p.peek(), path.text(), opList(comparisons))
	}
	values, err := p.values()
	if err != nil {
		return Atom{}, err
	}
	return p.finish(path, op, values)
}

func (p *parser) op() (Op, bool) {
	for _, op := range comparisons {
		if strings.HasPrefix(p.rest(), string(op)) {
			p.pos += len(op)
			return op, true
		}
	}
	return "", false
}

func (p *parser) name() string {
	start := p.pos
	for !p.eof() {
		r := p.peek()
		if r != '_' && !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') {
			break
		}
		p.pos++
	}
	return p.src[start:p.pos]
}

func (p *parser) path(t *Table) (Path, error) {
	var path Path
	table := t
	if p.consume('/') {
		if p.schema.Anchor == nil {
			return Path{}, fmt.Errorf("a leading / starts nowhere here: there is no workspace to read")
		}
		path.anchored, table = true, p.schema.Anchor
	}
	for {
		name := p.name()
		if name == "" {
			if p.eof() {
				return Path{}, fmt.Errorf("a path ends where a field name should be")
			}
			return Path{}, fmt.Errorf("expected a field name at %q", p.rest())
		}
		if len(path.steps) > 0 && name == "count" {
			last := path.Last()
			if !last.kind.rule().list {
				return Path{}, fmt.Errorf("%s/count: count counts a list, and %s is one %s", path.text(), last.name, last.kind.rule().name)
			}
			path.count = true
			if !p.boundary() && !p.atOp() {
				return Path{}, fmt.Errorf("%s/count: count ends a path", path.text())
			}
			return path, nil
		}
		if table == nil {
			return Path{}, fmt.Errorf("%s/%s: %s has no fields to read", path.text(), name, path.Last().name)
		}
		f := table.field(name)
		if f == nil {
			return Path{}, fmt.Errorf("unknown field %q; the fields of %s are %s", name, table.name, fieldList(table))
		}
		step := Step{field: f}
		if p.consume('(') {
			if f.of == nil {
				return Path{}, fmt.Errorf("%s(...): only a field whose values are rows takes a selector, and %s is %s", f.name, f.name, f.kind.rule().name)
			}
			where, err := p.selector(f.of, true)
			if err != nil {
				return Path{}, err
			}
			p.consume(')')
			step.where = where
		}
		path.steps = append(path.steps, step)
		if !p.consume('/') {
			return path, nil
		}
		table = f.of
	}
}

func (p *parser) atOp() bool {
	return slices.ContainsFunc(comparisons, func(op Op) bool { return strings.HasPrefix(p.rest(), string(op)) })
}

func (p *parser) values() ([]string, error) {
	var out []string
	for {
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, value)
		if !p.consume(',') {
			break
		}
	}
	if !p.boundary() {
		return nil, fmt.Errorf("unexpected %q in a value; quote a value that holds spaces, commas, parentheses or quotes", p.peek())
	}
	return out, nil
}

func (p *parser) value() (string, error) {
	if p.consume('"') {
		var b strings.Builder
		for {
			if p.eof() {
				return "", fmt.Errorf("a quoted value is never closed")
			}
			switch r := p.next(); r {
			case '"':
				return b.String(), nil
			case '\\':
				if p.eof() {
					return "", fmt.Errorf("a quoted value ends in a lone \\")
				}
				escaped := p.next()
				if escaped != '"' && escaped != '\\' {
					return "", fmt.Errorf(`\%c is not an escape; a quoted value escapes only \" and \\`, escaped)
				}
				b.WriteRune(escaped)
			default:
				b.WriteRune(r)
			}
		}
	}
	start := p.pos
	for !p.eof() {
		r := p.peek()
		if unicode.IsSpace(r) || strings.ContainsRune(`,()"`, r) {
			break
		}
		p.next()
	}
	return p.src[start:p.pos], nil
}

// finish checks op against what the path's kind admits and puts every value
// in the form its field compares in.
func (p *parser) finish(path Path, op Op, values []string) (Atom, error) {
	last := path.Last()
	r := last.kind.rule()
	if path.count {
		r = countRule
	}
	if !slices.Contains(r.ops, op) {
		return Atom{}, fmt.Errorf("%s is %s, which takes %s", path.text(), r.name, formList(r.ops))
	}
	if op == Present || op == Absent {
		if len(values) > 0 {
			return Atom{}, fmt.Errorf("%s takes no values", formOf(op))
		}
		return Atom{path: path, test: Presence{Op: op}}, nil
	}
	if len(values) == 0 {
		return Atom{}, fmt.Errorf("%s%s needs a value", path.text(), op)
	}
	if slices.Contains(orderings, op) && len(values) > 1 {
		return Atom{}, fmt.Errorf("%s%s takes one value, not %d", path.text(), op, len(values))
	}
	if slices.Contains(values, "") {
		return Atom{}, fmt.Errorf("%s%s has an empty value; !%s is the path that reads nothing", path.text(), op, path.text())
	}
	if path.count {
		counts := make([]int, 0, len(values))
		for _, value := range values {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return Atom{}, fmt.Errorf("%s%s%s: a count is a whole number", path.text(), op, value)
			}
			counts = append(counts, n)
		}
		return Atom{path: path, test: Count{Op: op, Values: counts}}, nil
	}
	canonical := make([]string, 0, len(values))
	for _, value := range values {
		c, err := p.canonical(last, op, value)
		if err != nil {
			return Atom{}, fmt.Errorf("%s%s%s: %v", path.text(), op, value, err)
		}
		canonical = append(canonical, c)
	}
	return Atom{path: path, test: Match{Op: op, Values: canonical}}, nil
}

// canonical is value as op compares it. A fragment ~ looks for is no member of
// anything, so it is compared as typed, ignoring case; every other value is a
// whole value of its field, in the field's own spelling.
func (p *parser) canonical(f *Field, op Op, value string) (string, error) {
	switch {
	case op == Contains:
		return strings.ToLower(value), nil
	case f.kind == Stamp:
		t, err := p.instant(value)
		if err != nil {
			return "", err
		}
		return model.StampText(t), nil
	default:
		return f.canonical(value)
	}
}

// stampUnits are the spans a stamp written from now counts in.
var stampUnits = map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}

// instant reads a stamp: now, now plus or minus a count of a unit, or an
// instant in RFC 3339.
func (p *parser) instant(value string) (time.Time, error) {
	offset, relative := strings.CutPrefix(value, "now")
	if !relative {
		t, err := model.ParseStamp(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("a stamp is now, now-6h, now+2d (m h d w) or an instant like 2026-01-02T15:04:05Z")
		}
		return t, nil
	}
	if offset == "" {
		return p.now, nil
	}
	refusal := fmt.Errorf("a stamp from now is now-N or now+N with a unit of m, h, d or w, like now-6h")
	parts := stampOffset.FindStringSubmatch(offset)
	if parts == nil {
		return time.Time{}, refusal
	}
	span, known := stampUnits[parts[3][0]]
	n, err := strconv.ParseInt(parts[2], 10, 64)
	if !known || err != nil || n > math.MaxInt64/int64(span) {
		return time.Time{}, refusal
	}
	if parts[1] == "-" {
		n = -n
	}
	return p.now.Add(time.Duration(n) * span), nil
}

// stampOffset is how far from now a stamp is: a sign, a whole number, and one
// character naming its unit in stampUnits.
var stampOffset = regexp.MustCompile(`^([-+])([0-9]+)(.)$`)

func (path Path) text() string {
	var b strings.Builder
	if path.anchored {
		b.WriteByte('/')
	}
	for i, step := range path.steps {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(step.field.name)
		if len(step.where.atoms) > 0 {
			b.WriteString("(...)")
		}
	}
	if path.count {
		b.WriteString("/count")
	}
	return b.String()
}

func fieldList(t *Table) string {
	names := make([]string, 0, len(t.fields))
	for _, f := range t.fields {
		names = append(names, f.name)
	}
	return strings.Join(names, ", ")
}

func opList(ops []Op) string {
	names := make([]string, 0, len(ops))
	for _, op := range ops {
		names = append(names, string(op))
	}
	return strings.Join(names, " ")
}

// formOf is how an operator is written in an atom, presence included.
func formOf(op Op) string {
	switch op {
	case Present:
		return "path"
	case Absent:
		return "!path"
	default:
		return "path" + string(op) + "value"
	}
}

func formList(ops []Op) string {
	forms := make([]string, 0, len(ops))
	for _, op := range ops {
		switch op {
		case Present, Absent:
			forms = append(forms, formOf(op))
		default:
			forms = append(forms, string(op))
		}
	}
	return strings.Join(forms, " ")
}
