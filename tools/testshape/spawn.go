package main

import (
	"fmt"
	"go/constant"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"
)

// program is where a spawned program came from: programNamed,
// programSelf or programUnresolved. [LAW:types-are-the-program]
type program interface{ program() }

// programNamed is a program a constant names, or a lookup of one.
type programNamed struct{ name string }

// programSelf is the running test binary; name says what named it
// (os.Executable(), os.Args[0]).
type programSelf struct{ name string }

// programUnresolved is any other program; why says what the resolution
// stopped at.
type programUnresolved struct{ why string }

func (programNamed) program()      {}
func (programSelf) program()       {}
func (programUnresolved) program() {}

func unresolved(why string) sources {
	return sources{first: programUnresolved{why}}
}

// sources is every source a spawn's program may come from: one at least.
// [LAW:types-are-the-program] no resolution can settle a spawn to no
// program, and so to no touch.
type sources struct {
	first program
	more  []program
}

func (s sources) and(o sources) sources {
	s.more = append(append(slices.Clone(s.more), o.first), o.more...)
	return s
}

func (s sources) all(yield func(program) bool) {
	if !yield(s.first) {
		return
	}
	for _, p := range s.more {
		if !yield(p) {
			return
		}
	}
}

// spawnSite is a spawn the walk reached: in the function that spawns, by the
// spawner it calls or stores into, running program.
type spawnSite struct {
	in      *ssa.Function
	spawner string
	program spawnProgram
}

// spawnProgram is a spawn's program: a value, resolved once the walk has
// every call site in its reach, or unknown.
type spawnProgram interface {
	sources(w *walker) sources
}

type programValue struct{ v ssa.Value }

type programUnknown struct{ why string }

func (p programValue) sources(w *walker) sources {
	return w.programs(p.v, map[ssa.Value]bool{})
}

func (p programUnknown) sources(*walker) sources { return unresolved(p.why) }

// programs resolves a spawn's program value to its sources, through
// parameters (at the static call sites in the walk's reach) and closure
// bindings, to what the classification says names a program. This is the
// only value the measure follows; every other shape, and a value that
// depends on itself, is unresolved. A parameter of a function the walk
// reached other than by a static call (a dynamic or interface call, a
// dispatch, a callback) is unresolved too, beside its static call sites'
// arguments: that caller may pass anything. path holds the values the
// resolution is inside.
func (w *walker) programs(v ssa.Value, path map[ssa.Value]bool) sources {
	if path[v] {
		return unresolved(fmt.Sprintf("%s, which depends on itself", v.Name()))
	}
	path[v] = true
	defer delete(path, v)
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			return sources{first: programNamed{constant.StringVal(v.Value)}}
		}
	case *ssa.Phi:
		return w.each(v.Edges, path)
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok && v.Index == 0 {
			if s, ok := w.calledProgram(call, path); ok {
				return s
			}
		}
	case *ssa.Call:
		if s, ok := w.calledProgram(v, path); ok {
			return s
		}
	case *ssa.UnOp:
		if name, ok := w.arg0(v); ok {
			return sources{first: programSelf{name}}
		}
	case *ssa.Parameter:
		fn := v.Parent()
		i := slices.Index(fn.Params, v)
		var s []sources
		for site := range w.sites.group(fn) {
			s = append(s, w.programs(site.common.Args[i], path))
		}
		if how, ok := w.indirect.get(fn); ok {
			s = append(s, unresolved(fmt.Sprintf("parameter %s of %s, which a call other than a static one may pass anything: %s", v.Name(), w.a.name(fn), how.text(relativeTo(w.a.mod)))))
		}
		if len(s) == 0 {
			return unresolved(fmt.Sprintf("parameter %s of %s, which no call in this reach passes", v.Name(), w.a.name(fn)))
		}
		return joinSources(s)
	case *ssa.FreeVar:
		fn := v.Parent()
		i := slices.Index(fn.FreeVars, v)
		var bound []ssa.Value
		for _, b := range fn.Parent().Blocks {
			for _, instr := range b.Instrs {
				if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == fn {
					bound = append(bound, mc.Bindings[i])
				}
			}
		}
		if len(bound) == 0 {
			return unresolved(fmt.Sprintf("free variable %s of %s, which no closure in its parent binds", v.Name(), w.a.name(fn)))
		}
		return w.each(bound, path)
	}
	return unresolved(fmt.Sprintf("%s at %s", v, w.a.prog.Fset.Position(v.Pos())))
}

// each is the sources of every value of a non-empty list.
func (w *walker) each(values []ssa.Value, path map[ssa.Value]bool) sources {
	s := make([]sources, len(values))
	for i, v := range values {
		s[i] = w.programs(v, path)
	}
	return joinSources(s)
}

// joinSources is every source of a non-empty list of sources.
func joinSources(s []sources) sources {
	out := s[0]
	for _, o := range s[1:] {
		out = out.and(o)
	}
	return out
}

// calledProgram is what a call's first result names as a program, as its
// callee's classification says: the test binary (program:self), or what
// its first argument names (program:lookup).
func (w *walker) calledProgram(call *ssa.Call, path map[ssa.Value]bool) (sources, bool) {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return sources{}, false
	}
	o := w.a.owner(callee)
	e, ok := o.knows.(Effect)
	if !ok {
		return sources{}, false
	}
	switch e.Names {
	case NamesSelf:
		return sources{first: programSelf{o.name.String() + "()"}}, true
	case NamesLookup:
		return w.programs(call.Call.Args[0], path), true
	}
	return sources{}, false
}

// arg0 matches the load of element 0 of a variable the classification says
// names the test binary (os.Args): *(&(*v)[0]).
func (w *walker) arg0(v *ssa.UnOp) (string, bool) {
	if v.Op != token.MUL {
		return "", false
	}
	ia, ok := v.X.(*ssa.IndexAddr)
	if !ok {
		return "", false
	}
	if c, ok := ia.Index.(*ssa.Const); !ok || c.Int64() != 0 {
		return "", false
	}
	load, ok := ia.X.(*ssa.UnOp)
	if !ok || load.Op != token.MUL {
		return "", false
	}
	g, ok := load.X.(*ssa.Global)
	if !ok || g.Pkg == nil || !w.a.class.isArg0(g.Pkg.Pkg.Path(), g.Name()) {
		return "", false
	}
	return g.Pkg.Pkg.Path() + "." + g.Name() + "[0]", true
}

// settle turns every spawn in the walk's reach, its binary's setup's
// included, into touches, each resolved through every call site the walk
// holds — the witness saying which program, or why it is unresolved.
func (w *walker) settle(setupRunsCLI bool) {
	for s := range w.spawns.all {
		for p := range s.program.sources(w).all {
			for t, what := range w.programTouches(p, setupRunsCLI) {
				w.touch(t, s.in, func() string { return fmt.Sprintf("%s spawns %s", s.spawner, what) })
			}
		}
	}
}

// programTouches is the one place a program becomes touches: git when it is
// git; a CLI run when it is the test binary and the binary's setup runs the
// CLI; another process otherwise. An unresolved program may be any program:
// another process, and, where the setup runs the CLI, the test binary run as
// the CLI, so it is charged both, the reading that errs furthest from unit.
func (w *walker) programTouches(p program, setupRunsCLI bool) func(yield func(Touch, string) bool) {
	return func(yield func(Touch, string) bool) {
		switch p := p.(type) {
		case programNamed:
			if w.a.class.isGit(p.name) {
				yield(Git, fmt.Sprintf("%q, which is git", p.name))
				return
			}
			yield(Proc, fmt.Sprintf("%q, another process", p.name))
		case programSelf:
			if setupRunsCLI {
				yield(CLI, fmt.Sprintf("%s, the test binary, whose setup runs the CLI", p.name))
				return
			}
			yield(Proc, fmt.Sprintf("%s, the test binary, whose setup does not reach the CLI: another process", p.name))
		case programUnresolved:
			if !yield(Proc, "an unresolved program ("+p.why+"): another process") || !setupRunsCLI {
				return
			}
			yield(CLI, "an unresolved program ("+p.why+"), which may be the test binary, whose setup runs the CLI")
		}
	}
}
