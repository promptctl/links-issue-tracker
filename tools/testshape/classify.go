package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Touch is one thing outside the process a test can reach. Any touch takes a
// test out of the unit layer.
type Touch int

const (
	Store Touch = iota // a store on disk
	Git
	CLI // a run of the CLI in another process
	Proc
	Net
	FS
	Clock
	numTouches
)

var touchNames = [numTouches]string{"store", "git", "cli", "proc", "net", "fs", "clock"}

func (t Touch) String() string { return touchNames[t] }

// Touches is a set of touches, one bit per touch.
type Touches uint8

// The set holds every touch: one past its width does not compile.
const _ Touches = 1 << (numTouches - 1)

func (s Touches) Has(t Touch) bool        { return s&(1<<t) != 0 }
func (s Touches) With(t Touch) Touches    { return s | 1<<t }
func (s Touches) Union(o Touches) Touches { return s | o }
func (s Touches) Each(yield func(Touch) bool) {
	for t := range numTouches {
		if s.Has(t) && !yield(t) {
			return
		}
	}
}

func (s Touches) String() string {
	var names []string
	for t := range s.Each {
		names = append(names, t.String())
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

// Class is what the classification says of one outside function, type,
// field or variable: an Effect, a SpawnArg, a SpawnField or a ProgramArg0.
// [LAW:types-are-the-program]
type Class interface{ class() }

// Call is the class of a function: an Effect or a SpawnArg.
type Call interface {
	Class
	knowledge
}

// Effect is what a call can do: the touches it can make, and what it calls
// back. The empty Effect is a call that touches nothing.
type Effect struct {
	Touches Touches
	// Dispatching: it calls back what was registered with it or handed to
	// it, so a call reaches every address-taken function and every method of
	// every runtime module type.
	Dispatching bool
	// Calls: the interfaces whose methods it calls back on the values
	// handed to it that implement them (fmt calls a fmt.Stringer's String),
	// each named importpath.Name, or error. A call reaches those methods of
	// every runtime type implementing the interface: an outside type's,
	// charged by that type's own classification, and a module type's,
	// walked.
	Calls []string
	// Names: what the call's first result names as a program to spawn.
	Names Names
}

// Read is the class of an outside function or method read in its source:
// what a call of it does, and, for a generic, what it does with its type
// arguments. Only the read section of the data file makes one, and Validate
// holds it to the source its module's pin names. [LAW:parse-dont-validate]
// what only a reading can say exists only here.
type Read struct {
	Effect   Effect
	TypeArgs TypeArgs
}

// TypeArgs is what a generic outside function does with the values of its
// type arguments.
type TypeArgs int

const (
	// TypeArgsConverted, every generic's unless read otherwise: it calls
	// its constraint's methods on them, and it may convert them to an
	// interface, as any outside code converting them, so they are runtime
	// types.
	TypeArgsConverted TypeArgs = iota
	// TypeArgsConstraint, "typeargs:constraint": it converts none of them
	// to an interface, and calls on them only its constraint's methods.
	TypeArgsConstraint
)

// calls is what the generic does with its type argument's values, as a
// witness says it.
func (t TypeArgs) calls() string {
	switch t {
	case TypeArgsConverted:
		return "calls its constraint's methods on"
	case TypeArgsConstraint:
		return "calls only its constraint's methods on"
	}
	panic(fmt.Sprintf("TypeArgs %d", t))
}

// Names is what a call's first result names as a program to spawn.
type Names int

const (
	NamesNothing Names = iota // a program it names is not known
	// NamesSelf, "program:self": the running test binary (os.Executable).
	NamesSelf
	// NamesLookup, "program:lookup": the program its first argument names
	// (exec.LookPath).
	NamesLookup
)

// SpawnArg: a call spawns the program passed as its argument Arg.
type SpawnArg struct{ Arg int }

// SpawnField: storing into the field names the program a spawn runs
// (exec.Cmd's Path).
type SpawnField struct{}

// ProgramArg0, "program:arg0": the variable is a []string whose element 0
// names the running test binary (os.Args).
type ProgramArg0 struct{}

func (Effect) class()      {}
func (Read) class()        {}
func (SpawnArg) class()    {}
func (SpawnField) class()  {}
func (ProgramArg0) class() {}

// parseClass parses one classification of a function, type, field or
// variable: "spawn:N", "spawn:field", "program:arg0", or an Effect.
func parseClass(spec string) (Class, error) {
	if arg, ok := strings.CutPrefix(spec, "spawn:"); ok {
		if arg == "field" {
			return SpawnField{}, nil
		}
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("classification %q: spawn takes an argument index or \"field\"", spec)
		}
		return SpawnArg{Arg: n}, nil
	}
	if spec == "program:arg0" {
		return ProgramArg0{}, nil
	}
	return parseEffect(spec)
}

// parseRead parses a Read: an Effect naming no program, with
// "typeargs:constraint" in its list or alone.
func parseRead(spec string) (Read, error) {
	var r Read
	var effect []string
	for _, elem := range strings.Split(spec, ",") {
		if elem == "typeargs:constraint" {
			r.TypeArgs = TypeArgsConstraint
			continue
		}
		effect = append(effect, elem)
	}
	if len(effect) == 0 {
		return r, nil
	}
	e, err := parseEffect(strings.Join(effect, ","))
	if err == nil && e.Names != NamesNothing {
		err = fmt.Errorf("%q: a program's source is classified under functions", spec)
	}
	r.Effect = e
	return r, err
}

// parseEffect parses an Effect: "none", or a comma-separated list of touch
// names, "dispatching", "calls:Interface", and one of "program:self" and
// "program:lookup". A package's default and an initializer are Effects: a
// spawn is classified on the one function that makes it.
func parseEffect(spec string) (Effect, error) {
	if spec == "none" {
		return Effect{}, nil
	}
	var e Effect
	for _, elem := range strings.Split(spec, ",") {
		if names, ok := map[string]Names{"program:self": NamesSelf, "program:lookup": NamesLookup}[elem]; ok {
			if e.Names != NamesNothing {
				return Effect{}, fmt.Errorf("classification %q: a call's result names one program", spec)
			}
			e.Names = names
			continue
		}
		if iface, ok := strings.CutPrefix(elem, "calls:"); ok {
			if _, _, err := splitInterfaceName(iface); err != nil {
				return Effect{}, fmt.Errorf("classification %q: %w", spec, err)
			}
			e.Calls = append(e.Calls, iface)
			continue
		}
		if elem == "dispatching" {
			e.Dispatching = true
			continue
		}
		if elem == "typeargs:constraint" {
			return Effect{}, fmt.Errorf("classification %q: typeargs:constraint is read in a generic's source, and is classified under read", spec)
		}
		i := slices.Index(touchNames[:], elem)
		if i < 0 || Touch(i) == CLI {
			return Effect{}, fmt.Errorf("classification %q: %q is not a touch an outside function makes (store, git, proc, net, fs, clock), dispatching, calls:Interface, program:self, program:lookup or none", spec, elem)
		}
		e.Touches = e.Touches.With(Touch(i))
	}
	return e, nil
}

// classificationFile is the data file's shape.
type classificationFile struct {
	Note string `json:"note"`
	// CLI names the functions whose reach is the CLI, as importpath.Name: a
	// binary whose setup reaches one re-runs the test binary as the CLI.
	CLI []string `json:"cli"`
	// Git names the programs that are git, matched against a spawned
	// program's name or the last element of its path.
	Git []string `json:"git"`
	// Modules classifies each module the measured builds link a package of,
	// by what its packages' initializers can do: every one must have an
	// entry, so a dependency added later cannot bring an initializer in
	// unseen. "std" is the standard library.
	Modules  map[string]moduleClassEntry  `json:"modules"`
	Packages map[string]packageClassEntry `json:"packages"`
}

type moduleClassEntry struct {
	Note string `json:"note,omitempty"`
	Init string `json:"init"`
	// Pin names each source of the module that the functions read in its
	// packages were read in, as a build compiles it (see loaded.SourceOf).
	Pin []string `json:"pin,omitempty"`
}

// packageClassEntry is one package's classification, or a pattern's. An
// entry takes the default and the functions of the longest pattern covering
// it, each of its own keys overriding the pattern's; its note is its own.
type packageClassEntry struct {
	Note      string            `json:"note,omitempty"`
	Default   *string           `json:"default,omitempty"`
	Functions map[string]string `json:"functions,omitempty"`
	// Read classifies functions and methods read in their source, below
	// what the package's default would charge, each a Read.
	Read map[string]string `json:"read,omitempty"`
}

// Classification is the parsed data file: every outside function's class,
// the CLI's entry points, and git's names. [LAW:one-source-of-truth] it is
// the one home of what code outside the module can do.
type Classification struct {
	cli     map[funcName]bool
	git     []string
	modules map[string]Effect   // each module's initializers, by module path
	pins    map[string][]string // each pinned module's pin, sorted
	// entries holds each entry's own classes, which Validate checks;
	// packages each entry's classes with what it takes from the patterns
	// covering it, which Lookup reads. Both by import path, or a path
	// prefix ending "/...".
	entries  map[string]packageEntry
	packages map[string]packageClass
}

// packageEntry is an entry's own classes: its default, nil where it states
// none, and its members.
type packageEntry struct {
	def *Effect
	members
}

type packageClass struct {
	def Effect
	members
}

type members struct {
	functions map[string]Call // "Name", "Type", "Type.Method", or "init", an Effect
	fields    map[string]bool // "Type.Field" whose store names a spawn's program
	arg0      map[string]bool // "Name" of a variable whose element 0 names the test binary
}

// over is m with base's keys that m does not hold.
func (m members) over(base members) members {
	out := members{functions: maps.Clone(base.functions), fields: maps.Clone(base.fields), arg0: maps.Clone(base.arg0)}
	maps.Copy(out.functions, m.functions)
	maps.Copy(out.fields, m.fields)
	maps.Copy(out.arg0, m.arg0)
	return out
}

// funcName names a function as importpath + "." + key, where key is "Name"
// or "Type.Method".
type funcName struct{ pkg, key string }

func (f funcName) String() string { return f.pkg + "." + f.key }

// parseCLIName parses a CLI entry point, importpath.Name: the name is after
// the last dot, since an import path's last element may hold one
// (gopkg.in/yaml.v3).
func parseCLIName(s string) (funcName, error) {
	dot := strings.LastIndex(s, ".")
	if dot < 0 || strings.Contains(s[dot+1:], "/") {
		return funcName{}, fmt.Errorf("%q is not importpath.Name", s)
	}
	return funcName{s[:dot], s[dot+1:]}, nil
}

func parseClassification(data []byte) (*Classification, error) {
	if err := strictJSON(data); err != nil {
		return nil, fmt.Errorf("classification file: %w", err)
	}
	var f classificationFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("classification file: %w", err)
	}
	c := &Classification{cli: map[funcName]bool{}, git: f.Git, modules: map[string]Effect{}, pins: map[string][]string{}, entries: map[string]packageEntry{}, packages: map[string]packageClass{}}
	for path, e := range f.Modules {
		init, err := parseEffect(e.Init)
		if err != nil {
			return nil, fmt.Errorf("classification file: module %s: %w", path, err)
		}
		c.modules[path] = init
		if e.Pin != nil {
			if len(e.Pin) == 0 {
				return nil, fmt.Errorf("classification file: module %s: pin names no source", path)
			}
			c.pins[path] = slices.Sorted(slices.Values(e.Pin))
		}
	}
	for _, s := range f.CLI {
		n, err := parseCLIName(s)
		if err != nil {
			return nil, fmt.Errorf("classification file: cli: %w", err)
		}
		c.cli[n] = true
	}
	for path, e := range f.Packages {
		pe, err := parsePackageEntry(path, e)
		if err != nil {
			return nil, fmt.Errorf("classification file: %w", err)
		}
		c.entries[path] = pe
	}
	for path := range c.entries {
		if _, err := c.resolve(path); err != nil {
			return nil, fmt.Errorf("classification file: %w", err)
		}
	}
	return c, nil
}

// parsePackageEntry parses the entry at path's own classes.
func parsePackageEntry(path string, e packageClassEntry) (packageEntry, error) {
	pe := packageEntry{members: members{functions: map[string]Call{}, fields: map[string]bool{}, arg0: map[string]bool{}}}
	if e.Default != nil {
		if *e.Default == "" {
			return pe, fmt.Errorf("%s: default is empty; name a class, or leave it out to take the covering pattern's", path)
		}
		def, err := parseEffect(*e.Default)
		if err != nil {
			return pe, fmt.Errorf("%s: default: %w", path, err)
		}
		pe.def = &def
	}
	if path == builtin && len(e.Read) > 0 {
		return pe, fmt.Errorf("%s: what the universe declares has no source to read", path)
	}
	for key, spec := range e.Read {
		if _, both := e.Functions[key]; both {
			return pe, fmt.Errorf("%s.%s: classified both under functions and under read", path, key)
		}
		if key == "init" {
			return pe, fmt.Errorf("%s.init: read classifies what a call of a function or method does", path)
		}
		r, err := parseRead(spec)
		if err != nil {
			return pe, fmt.Errorf("%s.%s: %w", path, key, err)
		}
		pe.functions[key] = r
	}
	for key, spec := range e.Functions {
		if key == "init" {
			init, err := parseEffect(spec)
			if err != nil {
				return pe, fmt.Errorf("%s.init: %w", path, err)
			}
			pe.functions[key] = init
			continue
		}
		cl, err := parseClass(spec)
		if err != nil {
			return pe, fmt.Errorf("%s.%s: %w", path, key, err)
		}
		switch cl := cl.(type) {
		case SpawnField:
			pe.fields[key] = true
		case ProgramArg0:
			pe.arg0[key] = true
		case Call:
			pe.functions[key] = cl
		}
	}
	return pe, nil
}

// resolve is the entry at path's classes: its own over those of the
// longest pattern covering it, resolved first.
func (c *Classification) resolve(path string) (packageClass, error) {
	if pc, ok := c.packages[path]; ok {
		return pc, nil
	}
	e := c.entries[path]
	var pc packageClass
	if parent, ok := longestCovering(path, c.entries); ok {
		base, err := c.resolve(parent)
		if err != nil {
			return pc, err
		}
		pc = packageClass{def: base.def, members: e.members.over(base.members)}
	} else if e.def == nil {
		return pc, fmt.Errorf("%s: the entry states no default, and no pattern covering it states one", path)
	} else {
		pc.members = e.members
	}
	if e.def != nil {
		pc.def = *e.def
	}
	c.packages[path] = pc
	return pc, nil
}

// strictJSON refuses what decoding accepts without a word: a token after the
// top-level value, and a key twice in one object, of which decoding keeps
// the last; two keys are one as decoding matches a field's name, ignoring
// case. [LAW:no-silent-failure] either way an entry someone wrote would
// classify nothing.
func strictJSON(data []byte) error {
	type frame struct {
		keys    []string // nil for an array
		wantKey bool
	}
	var stack []*frame
	done := false
	valueEnded := func() {
		if len(stack) == 0 {
			done = true
		} else if top := stack[len(stack)-1]; top.keys != nil {
			top.wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if done {
			return fmt.Errorf("%v follows the top-level value, at byte %d", tok, dec.InputOffset())
		}
		if n := len(stack); n > 0 && stack[n-1].wantKey {
			if tok == json.Delim('}') {
				stack = stack[:n-1]
				valueEnded()
				continue
			}
			key := tok.(string)
			if i := slices.IndexFunc(stack[n-1].keys, func(k string) bool { return strings.EqualFold(k, key) }); i >= 0 {
				return fmt.Errorf("key %q appears twice in one object, as %q, at byte %d", key, stack[n-1].keys[i], dec.InputOffset())
			}
			stack[n-1].keys, stack[n-1].wantKey = append(stack[n-1].keys, key), false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{keys: []string{}, wantKey: true})
		case json.Delim('['):
			stack = append(stack, &frame{})
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
			valueEnded()
		default:
			valueEnded()
		}
	}
}

// packageFor is the classification of the package at path: its own entry,
// else the longest "/..." pattern that covers it.
func (c *Classification) packageFor(path string) (packageClass, bool) {
	if pc, ok := c.packages[path]; ok {
		return pc, true
	}
	pattern, ok := longestCovering(path, c.packages)
	return c.packages[pattern], ok
}

// longestCovering is the longest "/..." pattern among entries' keys, other
// than path itself, that covers path.
func longestCovering[V any](path string, entries map[string]V) (string, bool) {
	best, found := "", false
	for pattern := range entries {
		if pattern != path && covers(pattern, path) && len(pattern) > len(best) {
			best, found = pattern, true
		}
	}
	return best, found
}

// covers reports whether the "/..." pattern covers the package at path.
func covers(pattern, path string) bool {
	prefix, ok := strings.CutSuffix(pattern, "/...")
	return ok && (path == prefix || strings.HasPrefix(path, prefix+"/"))
}

// Lookup is what the classification knows of the outside function named by
// pkg and key ("Name", "Type.Method" or "init"): its own entry, else its
// type's, else its package's default, else nothing, which is unclassified.
func (c *Classification) Lookup(pkg, key string) knowledge {
	pc, ok := c.packageFor(pkg)
	if !ok {
		return unclassified{}
	}
	if cl, ok := pc.functions[key]; ok {
		return cl
	}
	if typ, _, isMethod := strings.Cut(key, "."); isMethod {
		if cl, ok := pc.functions[typ]; ok {
			return cl
		}
	}
	return pc.def
}

// initOf is what the classification knows of the initializer of the outside
// package at pkg, of module mod: its package's own init entry, else its
// module's, else nothing. A package's default classifies what the package's
// functions do when called, not what its initializer does.
func (c *Classification) initOf(pkg, mod string) knowledge {
	if pc, ok := c.packageFor(pkg); ok {
		if cl, ok := pc.functions["init"]; ok {
			return cl
		}
	}
	if cl, ok := c.modules[mod]; ok {
		return cl
	}
	return unclassified{}
}

// fieldSpawn reports whether storing into field of the named type at pkg
// spawns a program.
func (c *Classification) fieldSpawn(pkg, typ, field string) bool {
	pc, ok := c.packageFor(pkg)
	return ok && pc.fields[typ+"."+field]
}

// isArg0 reports whether element 0 of the variable name at pkg names the
// running test binary.
func (c *Classification) isArg0(pkg, name string) bool {
	pc, ok := c.packageFor(pkg)
	return ok && pc.arg0[name]
}

func (c *Classification) isCLI(n funcName) bool { return c.cli[n] }

// isGit reports whether a program named name is git.
func (c *Classification) isGit(name string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	return slices.Contains(c.git, base)
}

// splitInterfaceName splits a called-back interface's name, importpath.Name
// or error, into its package path ("" for the universe) and name.
func splitInterfaceName(s string) (path, name string, err error) {
	if s == "error" {
		return "", s, nil
	}
	dot := strings.LastIndex(s, ".")
	if dot <= 0 || strings.Contains(s[dot+1:], "/") || !token.IsExported(s[dot+1:]) {
		return "", "", fmt.Errorf("%q names no interface as importpath.Name or error", s)
	}
	return s[:dot], s[dot+1:], nil
}

// lookupInterface resolves a called-back interface's name in the packages
// pkgOf finds.
func lookupInterface(s string, pkgOf func(path string) *types.Package) (*types.Interface, bool) {
	path, name, err := splitInterfaceName(s)
	if err != nil {
		return nil, false
	}
	names := types.Universe
	if path != "" {
		pkg := pkgOf(path)
		if pkg == nil {
			return nil, false
		}
		names = pkg.Scope()
	}
	tn, ok := names.Lookup(name).(*types.TypeName)
	if !ok {
		return nil, false
	}
	iface, ok := tn.Type().Underlying().(*types.Interface)
	return iface, ok
}

// builtin is the entry for what the universe declares: error's Error method,
// keyed "error.Error", as Go's documentation of the predeclared identifiers
// names them.
const builtin = "builtin"

// Validate checks every name in the classification against what the suite's
// loads hold. It refuses each entry that resolves to nothing — a package the
// suite does not load, a pattern that covers none, a function, type, method,
// field or variable no package it covers declares, a module no build links,
// a CLI entry point outside the suite's modules, whose code alone a walk
// reads — and each class its member cannot have; each linked module with no
// entry; each module whose packages hold a read function and that has no
// pin, or that a measured build compiles from a source its pin does not
// name; and each pin no read function needs. [LAW:no-silent-failure] an
// entry that names nothing classifies nothing, silently; it is dead, and the
// refusal says to delete it. A reading of other source than the build's
// classifies code nobody read.
func (c *Classification) Validate(s loadedSuite) error {
	universe, linked := s.packages, s.linked
	var problems []string
	for mod := range c.modules {
		if _, ok := linked[mod]; !ok {
			problems = append(problems, fmt.Sprintf("module %s: no measured build links a package of it; delete the entry", mod))
		}
	}
	for mod := range linked {
		if _, ok := c.modules[mod]; !ok {
			problems = append(problems, fmt.Sprintf("module %s: a measured build links its packages, and the classification has no entry for it", mod))
		}
	}
	read := map[string]bool{} // the modules whose packages hold a read function
	for path, pc := range c.entries {
		scopes := scopesOf(path, universe)
		if len(scopes) == 0 {
			problems = append(problems, fmt.Sprintf("%s: the suite loads no package it names; delete the entry", path))
			continue
		}
		check := func(key string, cl Class) {
			var wrong error
			for _, s := range scopes {
				found, err := checkMember(s, key, cl)
				if found && err == nil {
					return
				}
				if found {
					wrong = err
				}
			}
			if wrong != nil {
				problems = append(problems, fmt.Sprintf("%s.%s: %v", path, key, wrong))
				return
			}
			problems = append(problems, fmt.Sprintf("%s.%s: no package it names declares %s; delete the entry", path, key, key))
		}
		for key, cl := range pc.functions {
			check(key, cl)
		}
		for key := range pc.fields {
			check(key, SpawnField{})
		}
		for key := range pc.arg0 {
			check(key, ProgramArg0{})
		}
		calls := map[string]bool{}
		effects := slices.Collect(maps.Values(pc.functions))
		if pc.def != nil {
			effects = append(effects, *pc.def)
		}
		for _, cl := range effects {
			switch cl := cl.(type) {
			case Read:
				for _, sc := range scopes {
					read[s.moduleOf[sc.pkg.Path()]] = true
				}
				cl.Effect.callsInto(calls)
			case Effect:
				cl.callsInto(calls)
			}
		}
		for name := range calls {
			if _, ok := lookupInterface(name, func(p string) *types.Package { return universe[p] }); !ok {
				problems = append(problems, fmt.Sprintf("%s: calls:%s names no interface the suite loads", path, name))
			}
		}
	}
	for mod := range read {
		pin, ok := c.pins[mod]
		if !ok {
			problems = append(problems, fmt.Sprintf("module %s: its packages hold functions read in their source, and it has no pin naming the source they were read in", mod))
			continue
		}
		var unread []string
		for src := range linked[mod] {
			if !slices.Contains(pin, src) {
				unread = append(unread, src)
			}
		}
		if len(unread) > 0 {
			slices.Sort(unread)
			problems = append(problems, fmt.Sprintf("module %s: pinned at %s, and a measured build compiles it from %s; re-read there the source of each function read in its packages, and update its pin", mod, strings.Join(pin, ", "), strings.Join(unread, ", ")))
		}
	}
	for mod := range c.pins {
		if !read[mod] {
			problems = append(problems, fmt.Sprintf("module %s: a pin, and no package of it holds a function read in its source; delete the pin", mod))
		}
	}
	for n := range c.cli {
		p := universe[n.pkg]
		if p == nil {
			problems = append(problems, fmt.Sprintf("cli %s: the suite loads no package %s; delete the entry", n, n.pkg))
			continue
		}
		if !s.suite[s.moduleOf[n.pkg]] {
			problems = append(problems, fmt.Sprintf("cli %s: %s is not a package of the suite's modules, whose code alone a walk reads; delete the entry", n, n.pkg))
			continue
		}
		if _, ok := p.Scope().Lookup(n.key).(*types.Func); !ok {
			problems = append(problems, fmt.Sprintf("cli %s: %s declares no function %s; delete the entry", n, n.pkg, n.key))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the classification names what does not exist:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// scope is where a classification key resolves: a package, or the universe.
type scope struct {
	pkg   *types.Package // nil for the universe
	names *types.Scope
}

// scopesOf is every loaded scope the entry at path covers.
func scopesOf(path string, universe map[string]*types.Package) []scope {
	if path == builtin {
		return []scope{{names: types.Universe}}
	}
	var out []scope
	for p, pkg := range universe {
		if p == path || covers(path, p) {
			out = append(out, scope{pkg: pkg, names: pkg.Scope()})
		}
	}
	return out
}

// checkMember resolves key ("Name", "Type", "Type.Method", "Type.Field" or
// "init") in s. found is false when s declares no such member; err says how
// a found member contradicts cl.
func checkMember(s scope, key string, cl Class) (found bool, err error) {
	if key == "init" {
		// Every package has an initializer, and nothing can name it.
		return s.pkg != nil, nil
	}
	name, member, hasMember := strings.Cut(key, ".")
	obj := s.names.Lookup(name)
	if obj == nil {
		return false, nil
	}
	if !hasMember {
		if _, field := cl.(SpawnField); field {
			return true, fmt.Errorf("spawn:field names a field, as Type.Field")
		}
		if _, arg0 := cl.(ProgramArg0); arg0 {
			v, ok := obj.(*types.Var)
			if !ok || !types.Identical(v.Type(), types.NewSlice(types.Typ[types.String])) {
				return true, fmt.Errorf("program:arg0 names a []string variable")
			}
			return true, nil
		}
		switch o := obj.(type) {
		case *types.Func:
			return true, fits(cl, o)
		case *types.TypeName:
			if _, read := cl.(Read); read {
				return true, fmt.Errorf("read names each function and method it read, and %s is a type", name)
			}
			if e, call := cl.(Effect); !call || e.Names != NamesNothing {
				return true, fmt.Errorf("a type is neither a spawner nor a program's source")
			}
			return true, nil
		default:
			return true, fmt.Errorf("%s is neither a function nor a type", name)
		}
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return true, fmt.Errorf("%s is not a type", name)
	}
	if _, arg0 := cl.(ProgramArg0); arg0 {
		return true, fmt.Errorf("program:arg0 names a []string variable")
	}
	if _, field := cl.(SpawnField); field {
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			return false, nil
		}
		for i := range st.NumFields() {
			if st.Field(i).Name() == member {
				return true, nil
			}
		}
		return false, nil
	}
	// An interface's methods are not in its pointer's method set.
	recv := tn.Type()
	if !types.IsInterface(recv) {
		recv = types.NewPointer(recv)
	}
	m, _, _ := types.LookupFieldOrMethod(recv, true, s.pkg, member)
	fn, ok := m.(*types.Func)
	if !ok {
		return false, nil
	}
	return true, fits(cl, fn)
}

// fits refuses a spawn argument past fn's parameters; a program's source
// whose first result is not a string, or, for program:lookup, whose first
// parameter is not; and typeargs:constraint on what is no generic.
func fits(cl Class, fn *types.Func) error {
	sig := fn.Signature()
	isString := func(vars *types.Tuple) bool {
		return vars.Len() > 0 && types.Identical(vars.At(0).Type(), types.Typ[types.String])
	}
	switch cl := cl.(type) {
	case SpawnArg:
		if cl.Arg >= sig.Params().Len() {
			return fmt.Errorf("spawn argument is not a parameter of %s", fn.Name())
		}
	case Read:
		if cl.TypeArgs == TypeArgsConstraint && !isGeneric(fn) {
			return fmt.Errorf("typeargs:constraint classifies a generic, and %s is none", fn.Name())
		}
		return fits(cl.Effect, fn)
	case Effect:
		if cl.Names != NamesNothing && !isString(sig.Results()) {
			return fmt.Errorf("%s's first result is not a string, so it names no program", fn.Name())
		}
		if cl.Names == NamesLookup && !isString(sig.Params()) {
			return fmt.Errorf("%s's first parameter is not a string, so it looks no program up", fn.Name())
		}
	}
	return nil
}

// isGeneric reports whether fn is a generic function or a method of a
// generic type, instantiated or not.
func isGeneric(fn *types.Func) bool {
	sig := fn.Signature()
	if sig.TypeParams().Len() > 0 {
		return true
	}
	if sig.Recv() == nil {
		return false
	}
	recv := types.Unalias(sig.Recv().Type())
	if p, ok := recv.(*types.Pointer); ok {
		recv = types.Unalias(p.Elem())
	}
	n, ok := recv.(*types.Named)
	return ok && n.Origin().TypeParams().Len() > 0
}

// callsInto adds each interface e calls back to calls.
func (e Effect) callsInto(calls map[string]bool) {
	for _, name := range e.Calls {
		calls[name] = true
	}
}
