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
// So the state is layered: the binary's layer is the setup's reach, finished
// when the setup walk ends, and each test's walk adds its own layer over it.
// Each collection of the state is read through a set, which joins its two
// layers once: whatever either layer holds meets every call either has
// pending.

import (
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"
)

// store is one layer's part of one collection: its entries in the order they
// were added, each under a key, and optionally in a group (the address-taken
// functions of one signature, the static calls into one function).
type store[K comparable, E any] struct {
	index   map[K]int
	entries []E
	groups  map[any][]int
	// matches: the entries of a finished layer's store matching each
	// predicate asked of it, by the predicate's key. A finished layer is
	// never added to, so they stay true.
	matches map[any][]E
}

func newStore[K comparable, E any]() *store[K, E] {
	return &store[K, E]{index: map[K]int{}, groups: map[any][]int{}, matches: map[any][]E{}}
}

func (s *store[K, E]) put(g any, k K, e E) {
	s.index[k] = len(s.entries)
	s.groups[g] = append(s.groups[g], len(s.entries))
	s.entries = append(s.entries, e)
}

// set is one collection of the RTA state read through both layers: the
// binary's finished layer and the walk's own, which alone is added to.
// [LAW:one-source-of-truth] it is the one place the two layers are joined.
type set[K comparable, E any] struct{ binary, own *store[K, E] }

func (s set[K, E]) has(k K) bool {
	_, below := s.binary.index[k]
	_, mine := s.own.index[k]
	return below || mine
}

func (s set[K, E]) get(k K) (E, bool) {
	for _, st := range [2]*store[K, E]{s.binary, s.own} {
		if i, ok := st.index[k]; ok {
			return st.entries[i], true
		}
	}
	var zero E
	return zero, false
}

// add puts e under k in the walk's own layer, unless either layer holds k,
// and reports whether it did.
func (s set[K, E]) add(k K, e E) bool { return s.addTo(nil, k, e) }

// addTo is add, into group g.
func (s set[K, E]) addTo(g any, k K, e E) bool {
	if s.has(k) {
		return false
	}
	s.own.put(g, k, e)
	return true
}

// all yields every entry, the binary's first, as they stood when it began:
// what an entry's own handling adds meets the pending calls as it is added.
func (s set[K, E]) all(yield func(E) bool) {
	for _, st := range [2]*store[K, E]{s.binary, s.own} {
		for _, e := range st.entries[:len(st.entries):len(st.entries)] {
			if !yield(e) {
				return
			}
		}
	}
}

// group yields every entry in group g, the binary's first.
func (s set[K, E]) group(g any) func(func(E) bool) {
	return func(yield func(E) bool) {
		for _, st := range [2]*store[K, E]{s.binary, s.own} {
			idx := st.groups[g]
			for _, i := range idx[:len(idx):len(idx)] {
				if !yield(st.entries[i]) {
					return
				}
			}
		}
	}
}

// matching is every entry of either layer that keep accepts, the binary's
// first. key names keep: the binary's matches are found once per key.
func (s set[K, E]) matching(key any, keep func(E) bool) []E {
	below, ok := s.binary.matches[key]
	if !ok {
		for _, e := range s.binary.entries {
			if keep(e) {
				below = append(below, e)
			}
		}
		s.binary.matches[key] = below
	}
	out := slices.Clone(below)
	for _, e := range s.own.entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// layer is one layer of the RTA state, with what one walk reached.
type layer struct {
	steps    *store[*ssa.Function, step] // every reached function, with how
	indirect *store[*ssa.Function, via]  // those reached other than by a static call, by the first such reach
	touches  *store[Touch, touched]
	sites    *store[callSite, callSite] // static calls into module functions, grouped by callee
	spawns   *store[spawnSite, spawnSite]

	taken      *store[*ssa.Function, funcEntry] // grouped by normalized signature
	runtime    *store[int, typeEntry]           // by type
	structured *store[int, struct{}]            // the types whose structure a held value's walk followed
	converted  *store[int, struct{}]            // the types module code converted, whose methods it reached

	// The pending calls: each applies again to what either layer creates
	// after it.
	called     *store[int, pendingCall]        // by normalized signature: the first call of it
	invoked    *store[invoke, pendingInvoke]   // every interface call
	reflected  *store[int, pendingReflect]     // by interface: one in a converted value's structure
	callbacks  *store[int, pendingCallback]    // by interface: one an outside function calls back
	dispatched *store[struct{}, *ssa.Function] // the first dispatching function reached
	reflective *store[struct{}, pendingCall]   // the first unfollowed call that can be handed an interface
}

func newLayer() *layer {
	return &layer{
		steps:      newStore[*ssa.Function, step](),
		indirect:   newStore[*ssa.Function, via](),
		touches:    newStore[Touch, touched](),
		sites:      newStore[callSite, callSite](),
		spawns:     newStore[spawnSite, spawnSite](),
		taken:      newStore[*ssa.Function, funcEntry](),
		runtime:    newStore[int, typeEntry](),
		structured: newStore[int, struct{}](),
		converted:  newStore[int, struct{}](),
		called:     newStore[int, pendingCall](),
		invoked:    newStore[invoke, pendingInvoke](),
		reflected:  newStore[int, pendingReflect](),
		callbacks:  newStore[int, pendingCallback](),
		dispatched: newStore[struct{}, *ssa.Function](),
		reflective: newStore[struct{}, pendingCall](),
	}
}

// finished is a binary's layer once its setup walk has ended: what every
// test walk of the binary reads beneath its own. Only a setup walk's finish
// makes one, and it has no way to be added to.
type finished struct {
	layer *layer
}

// emptyBinary is the finished layer under a setup walk: nothing reached
// before it.
func emptyBinary() *finished { return &finished{layer: newLayer()} }

// touched is a touch with its first witness.
type touched struct {
	touch   Touch
	witness witness
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

// pendingCall is the first call of one kind: from where, and how, for the
// witness of what it reaches.
type pendingCall struct {
	from *ssa.Function
	how  string
}

// pendingReflect is an interface in a converted value's structure: it holds
// values converted elsewhere, which reflection may reach.
type pendingReflect struct {
	iface *types.Interface
	from  *ssa.Function
}

// pendingInvoke is a call of an interface method. A module method's call
// reaches it on every runtime type; an outside method's, on every runtime
// module type, the outside types' being charged by its classification.
type pendingInvoke struct {
	inv    invoke
	from   *ssa.Function
	module bool
}

// pendingCallback is an interface a reached outside function calls back on
// the values handed to it, named as its classification names it.
type pendingCallback struct {
	iface *types.Interface
	name  string
	from  *ssa.Function
}

func normalize(sig *types.Signature) *types.Signature {
	return types.NewSignatureType(nil, nil, nil, sig.Params(), sig.Results(), sig.Variadic())
}

// typeKey is t's key in the RTA state: one per type, identical types
// sharing it.
func (a *analyzer) typeKey(t types.Type) int {
	if k, ok := a.typeKeys.At(t).(int); ok {
		return k
	}
	k := a.typeKeys.Len()
	a.typeKeys.Set(t, k)
	return k
}

// sigKey is the key of every function of sig's parameters and results,
// whatever its receiver.
func (a *analyzer) sigKey(sig *types.Signature) int { return a.typeKey(normalize(sig)) }
