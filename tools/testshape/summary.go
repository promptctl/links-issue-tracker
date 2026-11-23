package main

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// summary is what one function's body does, in the terms the rules use. It
// is the same for every walk, so it is derived once per function.
type summary struct {
	static      []callSite
	taken       []*ssa.Function
	converted   []conversion
	dynamic     []dynamicCall
	invokes     []invoke
	spawnFields []spawnField
}

type callSite struct {
	caller *ssa.Function
	common *ssa.CallCommon
}

// dynamicCall is a call of a function value of signature sig, which can
// call any address-taken function of it. A function value handed to code
// that is not module code's to walk is such a call too, since that code may
// call it (t.Run, sort.Slice): how says which.
type dynamicCall struct {
	sig *types.Signature
	how string // "dynamic call", or "call into outside code"
}

// conversion is a value of a concrete type made into an interface value.
type conversion struct {
	typ types.Type // the concrete type
	to  *types.Interface
}

// invoke is a call of an interface method.
type invoke struct {
	iface  *types.Interface // the interface the call is made through
	method *types.Func
}

// spawnField is a program stored into a spawner's field (exec.Cmd's Path):
// the one way a spawn's program arrives other than as an argument. The store
// is read only to find the spawn and its program, never to decide reach.
type spawnField struct {
	spawner string
	program ssa.Value
}

func (a *analyzer) summary(fn *ssa.Function) *summary {
	if s, ok := a.summaries[fn]; ok {
		return s
	}
	s := &summary{}
	a.summaries[fn] = s
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			// A value with no use creates nothing: `var _ I = (*T)(nil)`
			// converts and is never read.
			if v, ok := instr.(ssa.Value); ok && v.Referrers() != nil && len(*v.Referrers()) == 0 {
				if _, isCall := instr.(*ssa.Call); !isCall {
					continue
				}
			}
			var callee *ssa.Value
			if c, ok := instr.(ssa.CallInstruction); ok {
				common := c.Common()
				_, builtin := common.Value.(*ssa.Builtin)
				switch {
				case common.IsInvoke():
					s.invokes = append(s.invokes, invoke{common.Value.Type().Underlying().(*types.Interface), common.Method})
				case common.StaticCallee() != nil:
					s.static = append(s.static, callSite{fn, common})
					callee = &common.Value
				case !builtin:
					s.dynamic = append(s.dynamic, dynamicCall{common.Signature(), "dynamic call"})
				}
				// A module function's own body decides what it does with
				// a function value; a builtin (append) only stores it.
				if sc := common.StaticCallee(); !builtin && (sc == nil || !a.walked(sc)) {
					for _, arg := range common.Args {
						if sig, ok := arg.Type().Underlying().(*types.Signature); ok {
							s.dynamic = append(s.dynamic, dynamicCall{sig, "call into outside code"})
						}
					}
				}
			}
			for _, op := range instr.Operands(nil) {
				if f, ok := (*op).(*ssa.Function); ok && op != callee {
					s.taken = append(s.taken, f)
				}
			}
			switch v := instr.(type) {
			case *ssa.MakeInterface:
				s.converted = append(s.converted, conversion{v.X.Type(), v.Type().Underlying().(*types.Interface)})
			case *ssa.Store:
				if sf, ok := a.spawnFieldOf(v); ok {
					s.spawnFields = append(s.spawnFields, sf)
				}
			}
		}
	}
	return s
}

// spawnFieldOf is the spawn a store makes, if it stores into a field the
// classification names a spawner's program.
func (a *analyzer) spawnFieldOf(st *ssa.Store) (spawnField, bool) {
	fa, ok := st.Addr.(*ssa.FieldAddr)
	if !ok {
		return spawnField{}, false
	}
	named := namedOf(fa.X.Type())
	if named == nil || named.Obj().Pkg() == nil {
		return spawnField{}, false
	}
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		return spawnField{}, false
	}
	pkg, typ, field := named.Obj().Pkg().Path(), named.Obj().Name(), strct.Field(fa.Field).Name()
	if !a.class.fieldSpawn(pkg, typ, field) {
		return spawnField{}, false
	}
	return spawnField{spawner: pkg + "." + typ + "." + field, program: st.Val}, true
}
