package main

// The RTA state. Reach follows rapid type analysis (Bacon and Sweeney): a
// dynamic call reaches every address-taken function of its signature, and an
// interface call reaches that method of every runtime type implementing the
// interface — a type converted to an interface somewhere in the reach.
//
// golang.org/x/tools/go/callgraph/rta computes one reach for a whole program,
// walking every function it reaches. This measure needs one reach per test,
// each over the setup every test of its binary runs first, and it never
// walks code outside the module, which the classification charges instead.
// So the state is layered: the binary's layer is the setup's reach, closed
// when the setup walk ends, and each test's walk adds its own layer over it.
// A walk reads its two layers as one set, and whatever either creates meets
// every call either has pending.

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// layer is one layer of the RTA state, with what one walk reached.
type layer struct {
	// closed: the layer is a binary's, shared by every test walk, and
	// nothing adds to it.
	closed bool

	visited   map[*ssa.Function]step
	origin    map[*ssa.Function]string // the root each function was reached from
	touches   reached
	sitesInto map[*ssa.Function][]callSite // static calls into module functions
	spawns    []spawnSite

	addrTaken    []funcEntry
	isAddrTaken  map[*ssa.Function]bool
	bySig        typeutil.Map // normalized signature -> []int into addrTaken
	runtimeTypes []typeEntry
	isRuntime    typeutil.Map
	structured   typeutil.Map // the types whose structure a conversion followed

	// The pending calls: each applies again to what either layer creates
	// after it.
	dynCalls     typeutil.Map // normalized signature -> pendingDynamic, its first call
	invokes      []pendingInvoke
	invoked      map[invoke]bool
	reflected    []pendingReflect
	isReflected  map[*types.Interface]bool
	dispatcher   *ssa.Function // the first dispatching function reached
	callbacks    []pendingCallback
	isCalledBack map[*types.Interface]bool

	// implementers caches, per interface, the runtime types implementing
	// it, once the layer is closed.
	implementers typeutil.Map
}

// funcEntry is an address-taken function, with where it was made.
type funcEntry struct {
	fn    *ssa.Function
	where entryOrigin
}

// typeEntry is a runtime type, with where it was made.
type typeEntry struct {
	typ   types.Type
	where entryOrigin
}

// pendingDynamic is the first call of a function value of one signature.
type pendingDynamic struct {
	from *ssa.Function
	how  string
}

// pendingInvoke is a call of a module interface method.
type pendingInvoke struct {
	inv  invoke
	from *ssa.Function
}

// pendingReflect is an interface in a converted value's structure: it holds
// values converted elsewhere, which reflection may reach.
type pendingReflect struct {
	iface *types.Interface
	from  *ssa.Function
}

// pendingCallback is an interface a reached outside function calls back on
// the values handed to it, named as its classification names it.
type pendingCallback struct {
	iface *types.Interface
	name  string
	from  *ssa.Function
}

func newLayer() *layer {
	return &layer{
		visited:      map[*ssa.Function]step{},
		origin:       map[*ssa.Function]string{},
		touches:      reached{},
		sitesInto:    map[*ssa.Function][]callSite{},
		isAddrTaken:  map[*ssa.Function]bool{},
		invoked:      map[invoke]bool{},
		isReflected:  map[*types.Interface]bool{},
		isCalledBack: map[*types.Interface]bool{},
	}
}

// emptyBinary is the layer under a setup walk: nothing reached before it.
func emptyBinary() *layer {
	l := newLayer()
	l.closed = true
	return l
}

func normalize(sig *types.Signature) *types.Signature {
	return types.NewSignatureType(nil, nil, nil, sig.Params(), sig.Results(), sig.Variadic())
}

func (l *layer) addTaken(e funcEntry) {
	l.isAddrTaken[e.fn] = true
	key := normalize(e.fn.Signature)
	idx, _ := l.bySig.At(key).([]int)
	l.bySig.Set(key, append(idx, len(l.addrTaken)))
	l.addrTaken = append(l.addrTaken, e)
}

func (l *layer) addRuntime(e typeEntry) {
	l.isRuntime.Set(e.typ, true)
	l.runtimeTypes = append(l.runtimeTypes, e)
}

func (l *layer) withSignature(sig *types.Signature) []funcEntry {
	idx, _ := l.bySig.At(normalize(sig)).([]int)
	out := make([]funcEntry, len(idx))
	for i, j := range idx {
		out[i] = l.addrTaken[j]
	}
	return out
}

func (l *layer) implementing(iface *types.Interface) []typeEntry {
	if cached, ok := l.implementers.At(iface).([]typeEntry); ok {
		return cached
	}
	var out []typeEntry
	for _, e := range l.runtimeTypes {
		if types.Implements(e.typ, iface) {
			out = append(out, e)
		}
	}
	if l.closed {
		l.implementers.Set(iface, out)
	}
	return out
}
