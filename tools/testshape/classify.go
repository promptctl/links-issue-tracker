package main

import (
	"encoding/json"
	"fmt"
	"go/types"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
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

// Touches is a set of touches.
type Touches uint8

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

// Class is what a call to one function outside the module can do.
// [LAW:types-are-the-program] touch-free is the empty set; a spawner is a
// spawn spec, decided by its program, not by a touch it always makes.
type Class struct {
	Touches Touches
	// Dispatching: it calls back what was registered with it or handed to
	// it, so a call reaches every binary-level function value and type.
	Dispatching bool
	// Spawn, when set, says where the spawned program comes from.
	Spawn *Spawn
}

// Spawn locates a spawner's program: argument Arg of the call, or, for a
// field spawner (exec.Cmd's Path), the value stored into the field.
type Spawn struct {
	Arg   int
	Field bool
}

// parseClass parses one classification: "none", "dispatching", "spawn:N",
// "spawn:field", or a comma-separated list of touch names that may also
// include "dispatching".
func parseClass(spec string) (Class, error) {
	switch {
	case spec == "none":
		return Class{}, nil
	case spec == "spawn:field":
		return Class{Spawn: &Spawn{Field: true}}, nil
	case strings.HasPrefix(spec, "spawn:"):
		n, err := strconv.Atoi(strings.TrimPrefix(spec, "spawn:"))
		if err != nil || n < 0 {
			return Class{}, fmt.Errorf("classification %q: spawn takes an argument index or \"field\"", spec)
		}
		return Class{Spawn: &Spawn{Arg: n}}, nil
	}
	var c Class
	for _, name := range strings.Split(spec, ",") {
		if name == "dispatching" {
			c.Dispatching = true
			continue
		}
		i := slices.Index(touchNames[:], name)
		if i < 0 || Touch(i) == CLI {
			return Class{}, fmt.Errorf("classification %q: %q is not a touch an outside function makes (store, git, proc, net, fs, clock), dispatching, none or spawn", spec, name)
		}
		c.Touches = c.Touches.With(Touch(i))
	}
	return c, nil
}

// classificationFile is the data file's shape.
type classificationFile struct {
	Note string `json:"note"`
	// CLI names the functions whose reach is the CLI, as importpath.Name: a
	// TestMain that reaches one re-runs the test binary as the CLI.
	CLI []string `json:"cli"`
	// Git names the programs that are git, matched against a spawned
	// program's name or the last element of its path.
	Git      []string                     `json:"git"`
	Packages map[string]packageClassEntry `json:"packages"`
}

type packageClassEntry struct {
	Note      string            `json:"note,omitempty"`
	Default   string            `json:"default"`
	Functions map[string]string `json:"functions,omitempty"`
}

// Classification is the parsed data file: every outside function's class,
// the CLI's entry points, and git's names. [LAW:one-source-of-truth] it is
// the one home of what code outside the module can do.
type Classification struct {
	cli      map[funcName]bool
	git      []string
	packages map[string]packageClass // by import path, or a path prefix ending "/..."
}

type packageClass struct {
	def       Class
	functions map[string]Class // "Name", "Type", "Type.Method", or "Type.Field"
}

// funcName names a function as importpath + "." + key, where key is "Name"
// or "Type.Method".
type funcName struct{ pkg, key string }

func (f funcName) String() string { return f.pkg + "." + f.key }

func parseFuncName(s string) (funcName, error) {
	// The key starts after the import path's last slash, at its first dot.
	slash := strings.LastIndex(s, "/")
	dot := strings.Index(s[slash+1:], ".")
	if dot < 0 {
		return funcName{}, fmt.Errorf("%q is not importpath.Name", s)
	}
	return funcName{s[:slash+1+dot], s[slash+1+dot+1:]}, nil
}

func parseClassification(data []byte) (*Classification, error) {
	var f classificationFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("classification file: %w", err)
	}
	c := &Classification{cli: map[funcName]bool{}, git: f.Git, packages: map[string]packageClass{}}
	for _, s := range f.CLI {
		n, err := parseFuncName(s)
		if err != nil {
			return nil, fmt.Errorf("classification file: cli: %w", err)
		}
		c.cli[n] = true
	}
	for path, e := range f.Packages {
		def, err := parseClass(e.Default)
		if err != nil {
			return nil, fmt.Errorf("classification file: %s: %w", path, err)
		}
		pc := packageClass{def: def, functions: map[string]Class{}}
		for key, spec := range e.Functions {
			cl, err := parseClass(spec)
			if err != nil {
				return nil, fmt.Errorf("classification file: %s.%s: %w", path, key, err)
			}
			pc.functions[key] = cl
		}
		c.packages[path] = pc
	}
	return c, nil
}

// packageFor is the classification of the package at path: its own entry,
// else the longest "/..." pattern that covers it.
func (c *Classification) packageFor(path string) (packageClass, bool) {
	if pc, ok := c.packages[path]; ok {
		return pc, true
	}
	best, found := "", false
	for pattern := range c.packages {
		prefix, ok := strings.CutSuffix(pattern, "/...")
		if ok && (path == prefix || strings.HasPrefix(path, prefix+"/")) && len(pattern) > len(best) {
			best, found = pattern, true
		}
	}
	return c.packages[best], found
}

// Lookup is the class of the outside function named by pkg and key ("Name"
// or "Type.Method"): its own entry, else its type's, else its package's
// default. ok is false when nothing classifies it.
func (c *Classification) Lookup(pkg, key string) (Class, bool) {
	pc, ok := c.packageFor(pkg)
	if !ok {
		return Class{}, false
	}
	if cl, ok := pc.functions[key]; ok {
		return cl, true
	}
	if typ, _, isMethod := strings.Cut(key, "."); isMethod {
		if cl, ok := pc.functions[typ]; ok {
			return cl, true
		}
	}
	return pc.def, true
}

// fieldSpawn reports whether storing into field of the named type at pkg
// spawns a program.
func (c *Classification) fieldSpawn(pkg, typ, field string) bool {
	pc, ok := c.packageFor(pkg)
	if !ok {
		return false
	}
	cl, ok := pc.functions[typ+"."+field]
	return ok && cl.Spawn != nil && cl.Spawn.Field
}

func (c *Classification) isCLI(pkg, key string) bool { return c.cli[funcName{pkg, key}] }

// isGit reports whether a program named name is git.
func (c *Classification) isGit(name string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	return slices.Contains(c.git, base)
}

// Validate loads every package the classification names, from each module
// of the suite, and refuses any name that resolves to nothing in all of them:
// a package that does not load, a "/..." pattern no module of the build
// covers, a function, type, method or field its package does not declare, a
// spawn argument past the function's parameters. [LAW:no-silent-failure] a
// classification that names nothing would classify nothing, silently.
func (c *Classification) Validate(mods []module, buildFlags []string) error {
	var exact, patterns []string
	for path := range c.packages {
		if strings.HasSuffix(path, "/...") {
			patterns = append(patterns, path)
		} else {
			exact = append(exact, path)
		}
	}
	for n := range c.cli {
		exact = append(exact, n.pkg)
	}
	slices.Sort(exact)
	exact = slices.Compact(exact)
	sort.Strings(patterns)

	var modulePaths []string
	byPath := map[string]*packages.Package{}
	for _, m := range mods {
		cmd := exec.Command("go", append([]string{"list", "-m", "-f", "{{.Path}}"}, append(buildFlags, "all")...)...)
		cmd.Dir = m.Dir
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("listing the modules of %s: %w", m.Path, err)
		}
		modulePaths = append(modulePaths, strings.Fields(string(out))...)
		// From source, so unexported names (a main package's main) resolve.
		mode := packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo
		pkgs, err := packages.Load(&packages.Config{Dir: m.Dir, Mode: mode, BuildFlags: buildFlags}, exact...)
		if err != nil {
			return fmt.Errorf("loading classified packages from %s: %w", m.Path, err)
		}
		for _, p := range pkgs {
			if len(p.Errors) == 0 && p.Types != nil && byPath[p.PkgPath] == nil {
				byPath[p.PkgPath] = p
			}
		}
	}

	var problems []string
	for _, pattern := range patterns {
		prefix := strings.TrimSuffix(pattern, "/...")
		if !slices.ContainsFunc(modulePaths, func(mp string) bool {
			return mp == prefix || strings.HasPrefix(mp, prefix+"/") || strings.HasPrefix(prefix, mp+"/")
		}) {
			problems = append(problems, fmt.Sprintf("pattern %s covers no module of the build", pattern))
		}
	}
	for _, path := range exact {
		p := byPath[path]
		if p == nil {
			problems = append(problems, fmt.Sprintf("package %s does not load from any module of the suite", path))
			continue
		}
		for key, cl := range c.packages[path].functions {
			if err := checkMember(p.Types, key, cl); err != nil {
				problems = append(problems, fmt.Sprintf("%s.%s: %v", path, key, err))
			}
		}
	}
	for n := range c.cli {
		if p := byPath[n.pkg]; p != nil {
			if err := checkMember(p.Types, n.key, Class{}); err != nil {
				problems = append(problems, fmt.Sprintf("cli %s: %v", n, err))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the classification names what does not exist:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// checkMember resolves key ("Name", "Type", "Type.Method", "Type.Field") in
// pkg, and checks a spawn spec against what it names.
func checkMember(pkg *types.Package, key string, cl Class) error {
	name, member, hasMember := strings.Cut(key, ".")
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return fmt.Errorf("%s declares no %s", pkg.Path(), name)
	}
	if !hasMember {
		switch o := obj.(type) {
		case *types.Func:
			if cl.Spawn != nil && (cl.Spawn.Field || cl.Spawn.Arg >= o.Signature().Params().Len()) {
				return fmt.Errorf("spawn argument is not a parameter of %s", name)
			}
		case *types.TypeName:
			if cl.Spawn != nil {
				return fmt.Errorf("a type is not a spawner")
			}
		default:
			return fmt.Errorf("%s is neither a function nor a type", name)
		}
		return nil
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return fmt.Errorf("%s is not a type", name)
	}
	if cl.Spawn != nil && cl.Spawn.Field {
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok || !slices.ContainsFunc(structFields(st), func(f *types.Var) bool { return f.Name() == member }) {
			return fmt.Errorf("%s has no field %s", name, member)
		}
		return nil
	}
	// An interface's methods are not in its pointer's method set.
	recv := tn.Type()
	if !types.IsInterface(recv) {
		recv = types.NewPointer(recv)
	}
	m, _, _ := types.LookupFieldOrMethod(recv, true, pkg, member)
	fn, ok := m.(*types.Func)
	if !ok {
		return fmt.Errorf("%s has no method %s", name, member)
	}
	if cl.Spawn != nil && cl.Spawn.Arg >= fn.Signature().Params().Len() {
		return fmt.Errorf("spawn argument is not a parameter of %s", key)
	}
	return nil
}

func structFields(st *types.Struct) []*types.Var {
	fields := make([]*types.Var, st.NumFields())
	for i := range fields {
		fields[i] = st.Field(i)
	}
	return fields
}
