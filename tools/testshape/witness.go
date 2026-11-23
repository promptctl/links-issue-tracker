package main

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// step is how a walk first reached a function.
type step struct {
	from   *ssa.Function // nil at the walk's root
	via    via
	origin string // the root the function was reached from: a test, TestMain, an initializer
}

// via is how a walk reached a function: a static call, a type converted to
// an interface, or an entry of the RTA state.
// Its text is spelled only when a witness is printed, since a walk reaches
// far more functions than it reports.
type via interface {
	text(q types.Qualifier) string // "" for a static call
}

type staticCall struct{}

type converting struct{ typ types.Type }

// throughFunc is a reach through an address-taken function of the RTA
// state; how is what reached it ("dynamic call", "call into testing.T.Run",
// "dispatch").
type throughFunc struct {
	how   string
	where entryOrigin
}

// throughType is a reach through a runtime type of the RTA state; how is
// what reached it ("interface call", "reflection", "dispatch", "String
// callback").
type throughType struct {
	how   string
	typ   types.Type
	where entryOrigin
}

// typeArgument is a reach through a type handed to an outside generic as a
// type argument; why is the instantiation that decided it, and how
// ("nested/strs.Join[genericjoin.file] calls String of genericjoin.file
// through its constraint").
type typeArgument struct{ why string }

func (staticCall) text(types.Qualifier) string { return "" }
func (c converting) text(q types.Qualifier) string {
	return "converts " + types.TypeString(c.typ, q) + " to an interface"
}
func (t typeArgument) text(types.Qualifier) string { return t.why }
func (t throughFunc) text(types.Qualifier) string {
	return t.how + " reaches value " + t.where.text()
}
func (t throughType) text(q types.Qualifier) string {
	return t.how + " reaches " + types.TypeString(t.typ, q) + " " + t.where.text()
}

// entryOrigin is where an entry of the RTA state was made: the setup or the
// test that made it exist, and the function in whose body it was made.
type entryOrigin struct {
	origin, creator string
}

func (o entryOrigin) text() string { return "from " + o.origin + ", made in " + o.creator }

// witness is one chain of functions from a walk's root to a touch.
type witness struct {
	chain []string
	what  string // what made the touch, at the chain's end
}

func (w witness) String() string {
	return strings.Join(w.chain, " → ") + " ⇒ " + w.what
}

// chain is the path the walk took from its root to fn, through the binary's
// setup when the walk reached fn there.
func (w *walker) chain(fn *ssa.Function) []string {
	q := relativeTo(w.a.mod)
	var rev []string
	for fn != nil {
		st := w.stepOf(fn)
		name := w.a.name(fn)
		if t := st.via.text(q); t != "" {
			name += " [" + t + "]"
		}
		rev = append(rev, name)
		fn = st.from
	}
	out := make([]string, len(rev))
	for i, s := range rev {
		out[len(rev)-1-i] = s
	}
	return out
}

// name names fn for a witness, relative to its module.
func (a *analyzer) name(fn *ssa.Function) string {
	return strings.ReplaceAll(fn.String(), a.mod.Path+"/", "")
}

func relativeTo(m module) types.Qualifier {
	return func(p *types.Package) string {
		path := p.Path()
		if rel, ok := strings.CutPrefix(path, m.Path+"/"); ok {
			return rel
		}
		return path
	}
}
