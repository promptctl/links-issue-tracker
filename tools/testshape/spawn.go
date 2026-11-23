package main

import (
	"fmt"
	"go/constant"
	"go/token"
	"slices"

	"golang.org/x/tools/go/ssa"
)

// program is where a spawned program came from.
type program struct {
	kind programKind
	name string // programNamed and programSelf: the name, or what named the binary
	why  string // programUnresolved: what the resolution stopped at
}

type programKind int

const (
	programNamed      programKind = iota // a constant, or exec.LookPath of one
	programSelf                          // os.Executable() or os.Args[0]: the test binary
	programUnresolved                    // anything else
)

func unresolved(why string) sources {
	return sources{first: program{kind: programUnresolved, why: why}}
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
// bindings. This is the only value the measure follows; every other shape,
// and a value that depends on itself, is unresolved, which counts as another
// process. path holds the values the resolution is inside.
func (w *walker) programs(v ssa.Value, path map[ssa.Value]bool) sources {
	if path[v] {
		return unresolved(fmt.Sprintf("%s, which depends on itself", v.Name()))
	}
	path[v] = true
	defer delete(path, v)
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			return sources{first: program{kind: programNamed, name: constant.StringVal(v.Value)}}
		}
	case *ssa.Phi:
		return w.each(v.Edges, path)
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok && v.Index == 0 {
			if p, ok := w.calledProgram(call); ok {
				return sources{first: p}
			}
		}
	case *ssa.UnOp:
		if isOsArgs0(v) {
			return sources{first: program{kind: programSelf, name: "os.Args[0]"}}
		}
	case *ssa.Parameter:
		fn := v.Parent()
		i := slices.Index(fn.Params, v)
		var args []ssa.Value
		for _, site := range w.sites(fn) {
			args = append(args, site.common.Args[i])
		}
		if len(args) == 0 {
			return unresolved(fmt.Sprintf("parameter %s of %s, which no static call in this reach passes", v.Name(), w.a.name(fn)))
		}
		return w.each(args, path)
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
	s := w.programs(values[0], path)
	for _, v := range values[1:] {
		s = s.and(w.programs(v, path))
	}
	return s
}

// calledProgram is the program a call's first result names: os.Executable()
// names the test binary, exec.LookPath of a constant names that constant.
func (w *walker) calledProgram(call *ssa.Call) (program, bool) {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return program{}, false
	}
	switch w.a.owner(callee).name.String() {
	case "os.Executable":
		return program{kind: programSelf, name: "os.Executable()"}, true
	case "os/exec.LookPath":
		if c, ok := call.Call.Args[0].(*ssa.Const); ok && c.Value != nil && c.Value.Kind() == constant.String {
			return program{kind: programNamed, name: constant.StringVal(c.Value)}, true
		}
	}
	return program{}, false
}

// isOsArgs0 matches the load of os.Args[0]: *(&(*os.Args)[0]).
func isOsArgs0(v *ssa.UnOp) bool {
	if v.Op != token.MUL {
		return false
	}
	ia, ok := v.X.(*ssa.IndexAddr)
	if !ok {
		return false
	}
	if c, ok := ia.Index.(*ssa.Const); !ok || c.Int64() != 0 {
		return false
	}
	load, ok := ia.X.(*ssa.UnOp)
	if !ok || load.Op != token.MUL {
		return false
	}
	g, ok := load.X.(*ssa.Global)
	return ok && g.Pkg != nil && g.Pkg.Pkg.Path() == "os" && g.Name() == "Args"
}

// settle turns every spawn in the walk's reach, its binary's setup's
// included, into a touch, each resolved through every call site the walk
// holds: git when the program is git, a CLI run when it is the test binary
// and the binary's setup runs the CLI, and another process otherwise — the
// witness saying which program, or why it is unresolved.
func (w *walker) settle(setupRunsCLI bool) {
	for _, l := range w.layers {
		for _, s := range l.spawns {
			for p := range s.program.sources(w).all {
				t, what := w.programTouch(p, setupRunsCLI)
				w.touch(t, s.in, func() string { return fmt.Sprintf("%s spawns %s", s.spawner, what) })
			}
		}
	}
}

// programTouch is the one place a program becomes a touch.
func (w *walker) programTouch(p program, setupRunsCLI bool) (Touch, string) {
	switch {
	case p.kind == programNamed && w.a.class.isGit(p.name):
		return Git, fmt.Sprintf("%q, which is git", p.name)
	case p.kind == programNamed:
		return Proc, fmt.Sprintf("%q, another process", p.name)
	case p.kind == programSelf && setupRunsCLI:
		return CLI, fmt.Sprintf("%s, the test binary, whose setup runs the CLI", p.name)
	case p.kind == programSelf:
		return Proc, fmt.Sprintf("%s, the test binary, whose setup does not reach the CLI: another process", p.name)
	default:
		return Proc, "an unresolved program (" + p.why + "): another process"
	}
}
