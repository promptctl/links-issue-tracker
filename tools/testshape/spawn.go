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
	name string // programNamed: the name or path
	why  string // programUnresolved: what the resolution stopped at
}

type programKind int

const (
	programNamed      programKind = iota // a constant, or exec.LookPath of one
	programSelf                          // os.Executable() or os.Args[0]: the test binary
	programUnresolved                    // anything else
)

// programs resolves a spawn's program value to its sources, through
// parameters (at the static call sites the walk recorded) and closure
// bindings. [LAW:effects-at-boundaries] this is the only value the measure
// follows; every other shape is unresolved, which counts as another process.
func (w *walker) programs(v ssa.Value, seen map[ssa.Value]bool) []program {
	if seen[v] {
		return nil
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			return []program{{kind: programNamed, name: constant.StringVal(v.Value)}}
		}
	case *ssa.Phi:
		var out []program
		for _, e := range v.Edges {
			out = append(out, w.programs(e, seen)...)
		}
		return out
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok && v.Index == 0 {
			if p, ok := w.calledProgram(call); ok {
				return []program{p}
			}
		}
	case *ssa.UnOp:
		if isOsArgs0(v) {
			return []program{{kind: programSelf, name: "os.Args[0]"}}
		}
	case *ssa.Parameter:
		fn := v.Parent()
		i := slices.Index(fn.Params, v)
		sites := w.sitesInto[fn]
		if len(sites) == 0 {
			return []program{{kind: programUnresolved, why: fmt.Sprintf("parameter %s of %s, which no static call in this reach passes", v.Name(), w.a.name(fn))}}
		}
		var out []program
		for _, site := range sites {
			out = append(out, w.programs(site.common.Args[i], seen)...)
		}
		return out
	case *ssa.FreeVar:
		fn := v.Parent()
		i := slices.Index(fn.FreeVars, v)
		var out []program
		for _, b := range fn.Parent().Blocks {
			for _, instr := range b.Instrs {
				if mc, ok := instr.(*ssa.MakeClosure); ok && mc.Fn == fn {
					out = append(out, w.programs(mc.Bindings[i], seen)...)
				}
			}
		}
		return out
	}
	return []program{{kind: programUnresolved, why: fmt.Sprintf("%s at %s", v, w.a.prog.Fset.Position(pos(v)))}}
}

func pos(v ssa.Value) token.Pos {
	if instr, ok := v.(ssa.Instruction); ok {
		return instr.Pos()
	}
	return v.Pos()
}

// calledProgram is the program a call's first result names: os.Executable()
// names the test binary, exec.LookPath of a constant names that constant.
func (w *walker) calledProgram(call *ssa.Call) (program, bool) {
	callee := call.Call.StaticCallee()
	if callee == nil {
		return program{}, false
	}
	switch o := w.a.owner(callee); o.pkg + "." + o.key {
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

// settleSpawns turns every spawn the walk reached into a touch: git when the
// program is git, a CLI run when it is the test binary and the binary's
// TestMain reaches the CLI, and another process otherwise — the witness
// saying which program, or why it is unresolved.
func (w *walker) settleSpawns(testMainRunsCLI bool) {
	for _, s := range w.spawns {
		progs := []program{{kind: programUnresolved, why: "the spawner " + s.spawner + " is reached as a value"}}
		if s.resolved {
			progs = w.programs(s.program, map[ssa.Value]bool{})
		}
		for _, p := range progs {
			t, what := w.programTouch(p, testMainRunsCLI)
			w.touch(t, s.in, func() string { return fmt.Sprintf("%s spawns %s", s.spawner, what) })
		}
	}
}

func (w *walker) programTouch(p program, testMainRunsCLI bool) (Touch, string) {
	switch {
	case p.kind == programNamed && w.a.class.isGit(p.name):
		return Git, fmt.Sprintf("%q, which is git", p.name)
	case p.kind == programNamed:
		return Proc, fmt.Sprintf("%q, another process", p.name)
	case p.kind == programSelf && testMainRunsCLI:
		return CLI, fmt.Sprintf("%s, the test binary, whose TestMain runs the CLI", p.name)
	case p.kind == programSelf:
		return Proc, fmt.Sprintf("%s, the test binary, whose TestMain does not reach the CLI: another process", p.name)
	default:
		return Proc, "an unresolved program (" + p.why + "): another process"
	}
}
