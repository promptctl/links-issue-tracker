package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAClassParsesToAnEffectOrASpawn(t *testing.T) {
	for spec, want := range map[string]Class{
		"none":                             Effect{},
		"fs":                               Effect{Touches: Touches(0).With(FS)},
		"store,git":                        Effect{Touches: Touches(0).With(Store).With(Git)},
		"dispatching":                      Effect{Dispatching: true},
		"net,dispatching":                  Effect{Touches: Touches(0).With(Net), Dispatching: true},
		"calls:fmt.Stringer,calls:error":   Effect{Calls: []string{"fmt.Stringer", "error"}},
		"fs,calls:encoding/json.Marshaler": Effect{Touches: Touches(0).With(FS), Calls: []string{"encoding/json.Marshaler"}},
		"spawn:0":                          SpawnArg{Arg: 0},
		"spawn:2":                          SpawnArg{Arg: 2},
		"spawn:field":                      SpawnField{},
		"git,dispatching,net,proc,fs":      Effect{Touches: Touches(0).With(Git).With(Net).With(Proc).With(FS), Dispatching: true},
		"store,git,proc,net,fs,clock":      Effect{Touches: Touches(0).With(Store).With(Git).With(Proc).With(Net).With(FS).With(Clock)},
	} {
		got, err := parseClass(spec)
		if err != nil {
			t.Errorf("parseClass(%q) error = %v", spec, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseClass(%q) = %#v, want %#v", spec, got, want)
		}
	}
}

func TestAReadParsesToAnEffectAndWhatItDoesWithItsTypeArguments(t *testing.T) {
	for spec, want := range map[string]Read{
		"none":                      {},
		"typeargs:constraint":       {TypeArgs: TypeArgsConstraint},
		"none,typeargs:constraint":  {TypeArgs: TypeArgsConstraint},
		"fs,typeargs:constraint":    {Effect: Effect{Touches: Touches(0).With(FS)}, TypeArgs: TypeArgsConstraint},
		"calls:fmt.Stringer":        {Effect: Effect{Calls: []string{"fmt.Stringer"}}},
		"typeargs:constraint,clock": {Effect: Effect{Touches: Touches(0).With(Clock)}, TypeArgs: TypeArgsConstraint},
	} {
		got, err := parseRead(spec)
		if err != nil {
			t.Errorf("parseRead(%q) error = %v", spec, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseRead(%q) = %#v, want %#v", spec, got, want)
		}
	}
	for _, spec := range []string{"", "spawn:0", "spawn:field", "program:self", "fs,program:lookup", "typeargs:all"} {
		if _, err := parseRead(spec); err == nil {
			t.Errorf("parseRead(%q) accepted it", spec)
		}
	}
}

func TestAClassNamingNoTouchRefuses(t *testing.T) {
	// cli is a touch, but not one an outside function makes: a CLI run is
	// decided by what a spawn runs. A called-back interface is named as
	// importpath.Name, or error.
	for _, spec := range []string{"", "disk", "fs,", "cli", "spawn:", "spawn:-1", "spawn:x", "calls:", "calls:String", "calls:fmt.stringer", "calls:fmt/"} {
		if _, err := parseClass(spec); err == nil {
			t.Errorf("parseClass(%q) accepted it", spec)
		}
	}
}

func TestAClassificationFileRefusesWhatItCannotParse(t *testing.T) {
	for name, data := range map[string]string{
		"an unknown field":           `{"packages": {}, "extra": 1}`,
		"a bad default":              `{"packages": {"os": {"default": "disk"}}}`,
		"a field as a default":       `{"packages": {"os/exec": {"default": "spawn:field"}}}`,
		"a bad function":             `{"packages": {"os": {"default": "fs", "functions": {"Getenv": "nothing"}}}}`,
		"a cli without a .":          `{"cli": ["nowhere"], "packages": {}}`,
		"a cli naming a path":        `{"cli": ["example.com/a.b/c"], "packages": {}}`,
		"not a json object":          `[]`,
		"an unknown package key":     `{"packages": {"os": {"default": "fs", "notes": "x"}}}`,
		"a bad module init":          `{"modules": {"example.com/m": {"init": "disk"}}, "packages": {}}`,
		"a field as an init":         `{"modules": {"example.com/m": {"init": "spawn:field"}}, "packages": {}}`,
		"a spawn as an init":         `{"modules": {"example.com/m": {"init": "spawn:0"}}, "packages": {}}`,
		"a spawn as a default":       `{"packages": {"os": {"default": "spawn:0"}}}`,
		"a spawn as a package init":  `{"packages": {"os": {"default": "fs", "functions": {"init": "spawn:0"}}}}`,
		"arg0 as a default":          `{"packages": {"os": {"default": "program:arg0"}}}`,
		"two programs":               `{"packages": {"os": {"default": "fs", "functions": {"Executable": "program:self,program:lookup"}}}}`,
		"no default, no pattern":     `{"packages": {"os": {"functions": {"Getenv": "none"}}}}`,
		"an empty default":           `{"packages": {"os": {"default": ""}}}`,
		"typeargs unread":            `{"packages": {"slices": {"default": "none", "functions": {"Clone": "typeargs:constraint"}}}}`,
		"typeargs as a default":      `{"packages": {"slices": {"default": "typeargs:constraint"}}}`,
		"typeargs as an init":        `{"modules": {"std": {"init": "typeargs:constraint"}}, "packages": {}}`,
		"typeargs as a package init": `{"packages": {"slices": {"default": "none", "functions": {"init": "typeargs:constraint"}}}}`,
		"a pin naming no source":     `{"modules": {"std": {"init": "none", "pin": []}}, "packages": {}}`,
		"a key read and not":         `{"packages": {"slices": {"default": "none", "functions": {"Clone": "none"}, "read": {"Clone": "typeargs:constraint"}}}}`,
		"a spawn field read":         `{"packages": {"os/exec": {"default": "none", "read": {"Cmd.Path": "spawn:field"}}}}`,
		"a program read":             `{"packages": {"os": {"default": "fs", "read": {"Executable": "program:self"}}}}`,
		"an init read":               `{"packages": {"os": {"default": "fs", "read": {"init": "none"}}}}`,
		"a read under builtin":       `{"packages": {"builtin": {"default": "none", "read": {"error.Error": "none"}}}}`,
	} {
		if _, err := parseClassification([]byte(data)); err == nil || !strings.HasPrefix(err.Error(), "classification file: ") {
			t.Errorf("%s: parseClassification error = %v, want a classification file error", name, err)
		}
	}
}

func TestAClassificationFileRefusesWhatDecodingWouldDrop(t *testing.T) {
	for name, tc := range map[string]struct{ data, want string }{
		"a second value":             {`{"packages": {}} {"packages": {}}`, "classification file: { follows the top-level value"},
		"a trailing scalar":          {`{"packages": {}} 1`, "classification file: 1 follows the top-level value"},
		"a key twice":                {`{"packages": {"os": {"default": "fs"}, "os": {"default": "none"}}}`, `classification file: key "os" appears twice in one object`},
		"a function key twice":       {`{"packages": {"os": {"default": "fs", "functions": {"Getenv": "none", "Getenv": "fs"}}}}`, `classification file: key "Getenv" appears twice in one object`},
		"a top-level field twice":    {`{"git": ["git"], "packages": {}, "git": []}`, `classification file: key "git" appears twice in one object`},
		"a field twice in two cases": {`{"packages": {"os": {"default": "fs", "Default": "none"}}}`, `classification file: key "Default" appears twice in one object, as "default"`},
	} {
		if _, err := parseClassification([]byte(tc.data)); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: parseClassification error = %v, want one starting %q", name, err, tc.want)
		}
	}
	// The same key in two objects is two keys.
	if _, err := parseClassification([]byte(`{"packages": {"os": {"default": "fs", "functions": {"Exit": "none"}}, "os/exec": {"default": "none", "functions": {"Exit": "none"}}}}`)); err != nil {
		t.Errorf("parseClassification refused one key in two objects: %v", err)
	}
}

func TestACLINameSplitsAtTheLastDot(t *testing.T) {
	// An import path's last element may hold a dot.
	got, err := parseCLIName("gopkg.in/cli.v2.Run")
	if err != nil || got != (funcName{"gopkg.in/cli.v2", "Run"}) {
		t.Errorf(`parseCLIName("gopkg.in/cli.v2.Run") = %v, %v; want gopkg.in/cli.v2 . Run`, got, err)
	}
}

func TestLookupTakesTheMostSpecificEntry(t *testing.T) {
	class, err := parseClassification([]byte(`{
		"git": ["git"],
		"packages": {
			"os": {"default": "fs", "functions": {"Getenv": "none", "Process": "proc", "Process.Pid": "none", "init": "none"}},
			"os/exec": {"default": "none", "functions": {"Command": "spawn:0", "Cmd.Path": "spawn:field"}},
			"example.com/dolt/...": {"default": "store,git"},
			"example.com/dolt/utils/...": {"default": "fs", "functions": {"Sum": "none", "Node.String": "clock"}},
			"example.com/dolt/utils/hash": {"default": "none"},
			"example.com/dolt/utils/fs/tree": {"read": {"Node.String": "none", "Map": "typeargs:constraint"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	fs, storeGit := Effect{Touches: Touches(0).With(FS)}, Effect{Touches: Touches(0).With(Store).With(Git)}
	for _, tc := range []struct {
		pkg, key string
		want     knowledge
	}{
		{"os", "ReadFile", fs},     // the package default
		{"os", "Getenv", Effect{}}, // its own entry
		{"os", "init", Effect{}},   // the package's initializer
		{"os", "Process.Kill", Effect{Touches: Touches(0).With(Proc)}}, // its type's entry
		{"os", "Process.Pid", Effect{}},                                // its own entry over its type's
		{"os/exec", "Command", SpawnArg{0}},                            // a spawner
		{"example.com/dolt", "Open", storeGit},                         // a pattern covers its own path
		{"example.com/dolt/sql", "Open", storeGit},                     // and what is under it
		{"example.com/dolt/utils/fs", "Open", fs},                      // the longest pattern wins
		{"example.com/dolt/utils/hash", "Of", Effect{}},                // an exact entry over any pattern
		{"example.com/dolt/utils/fs/tree", "Walk", fs},                 // an entry with no default takes its pattern's
		{"example.com/dolt/utils/fs/tree", "Sum", Effect{}},            // and its functions
		{"example.com/dolt/utils/hash", "Sum", Effect{}},               // with a default of its own too
		{"example.com/dolt/utils/fs/tree", "Node.String", Read{}},      // its own keys over the pattern's
		{"example.com/dolt/utils/fs/tree", "Map", Read{TypeArgs: TypeArgsConstraint}},
		{"strings", "Cut", unclassified{}},           // nothing names it
		{"example.com/doltish", "F", unclassified{}}, // a pattern covers whole elements
	} {
		if got := class.Lookup(tc.pkg, tc.key); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Lookup(%s, %s) = %#v, want %#v", tc.pkg, tc.key, got, tc.want)
		}
	}
	if !class.fieldSpawn("os/exec", "Cmd", "Path") || class.fieldSpawn("os/exec", "Cmd", "Dir") {
		t.Error("fieldSpawn: only Cmd.Path names a spawn's program")
	}
}

func TestAnInitializerIsItsPackagesInitEntryElseItsModules(t *testing.T) {
	class, err := parseClassification([]byte(`{
		"modules": {"example.com/m": {"init": "none"}, "std": {"init": "clock"}},
		"packages": {
			"os": {"default": "fs"},
			"example.com/m/reads": {"default": "none", "functions": {"init": "fs"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pkg, mod string
		want     knowledge
	}{
		{"os", "std", Effect{Touches: Touches(0).With(Clock)}},                         // the module's, not the package default
		{"example.com/m/reads", "example.com/m", Effect{Touches: Touches(0).With(FS)}}, // the package's own init entry
		{"example.com/m/other", "example.com/m", Effect{}},                             // the module's
		{"example.com/x", "example.com/x", unclassified{}},                             // nothing names it
	} {
		if got := class.initOf(tc.pkg, tc.mod); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("initOf(%s, %s) = %#v, want %#v", tc.pkg, tc.mod, got, tc.want)
		}
	}
}

func TestGitIsMatchedByNameOrPathBase(t *testing.T) {
	class, err := parseClassification([]byte(`{"git": ["git"], "packages": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"git":              true,
		"/usr/bin/git":     true,
		"/opt/git/bin/git": true,
		"gitk":             false,
		"/usr/bin/git-lfs": false,
		"/git/bin/true":    false,
	} {
		if got := class.isGit(name); got != want {
			t.Errorf("isGit(%q) = %v, want %v", name, got, want)
		}
	}
}

// suiteFrom type-checks each source as the package at its path, of the
// module its path begins with ("example.com/app/..." is example.com/app's),
// the suite being app's module, linking the modules linked.
func suiteFrom(t *testing.T, linked map[string]map[string]bool, sources map[string]string) loadedSuite {
	t.Helper()
	fset := token.NewFileSet()
	s := loadedSuite{packages: map[string]*types.Package{}, moduleOf: map[string]string{}, suite: map[string]bool{"example.com/app": true}, linked: linked}
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path+".go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := (&types.Config{}).Check(path, fset, []*ast.File{f}, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.packages[path] = pkg
		parts := strings.SplitN(path, "/", 3)
		s.moduleOf[path] = parts[0] + "/" + parts[1]
	}
	return s
}

func TestValidateAcceptsEveryNameTheSuiteLoads(t *testing.T) {
	suite := suiteFrom(t, map[string]map[string]bool{"std": {"std@go1.26": true}, "example.com/tree": {"example.com/tree@v1.0.0": true}}, map[string]string{
		"example.com/run":      "package run; type Cmd struct{ Path string }; func Start(name string, args ...string) {}; func (*Cmd) Run() error { return nil }; var Args []string; func Self() (string, error) { return \"\", nil }; func Find(name string) (string, error) { return name, nil }",
		"example.com/tree/a":   "package a; func Open() {}; func Map[T any](x T) T { return x }; type Set[T comparable] map[T]bool; func (Set[T]) Add(T) {}",
		"example.com/tree/b":   "package b; type I interface{ Do() }",
		"example.com/app/main": "package main; func Main() {}",
	})
	class, err := parseClassification([]byte(`{
		"cli": ["example.com/app/main.Main"],
		"modules": {"std": {"init": "none"}, "example.com/tree": {"init": "none", "pin": ["example.com/tree@v0.9.0", "example.com/tree@v1.0.0"]}},
		"packages": {
			"builtin": {"default": "none", "functions": {"error.Error": "calls:error"}},
			"example.com/tree/a": {"read": {"Map": "typeargs:constraint", "Set.Add": "typeargs:constraint", "Open": "none"}},
			"example.com/run": {"default": "none", "functions": {"Start": "spawn:0", "Cmd.Run": "proc", "Cmd.Path": "spawn:field", "init": "none", "Args": "program:arg0", "Self": "program:self", "Find": "fs,program:lookup"}},
			"example.com/tree/...": {"default": "fs", "functions": {"Open": "calls:example.com/tree/b.I", "I.Do": "net"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := class.Validate(suite); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestValidateRefusesEveryDeadEntry(t *testing.T) {
	suite := suiteFrom(t, map[string]map[string]bool{"example.com/linked": {"example.com/linked@v1": true}, "example.com/tree": {"example.com/tree@v2": true, "./tree@h1:x": true}, "example.com/lone": {"example.com/lone@v1": true}}, map[string]string{
		"example.com/run":      "package run; type Cmd struct{ Path string }; func Start(name string) {}; var Count int; func Self() error { return nil }; func Find(n int) string { return \"\" }; func Main() {}",
		"example.com/tree/a":   "package a; func Open() {}; type Node struct{}; type Set[T any] struct{}",
		"example.com/lone/x":   "package x; func F() {}",
		"example.com/app/main": "package main; func Main() {}",
	})
	class, err := parseClassification([]byte(`{
		"cli": ["example.com/gone.Main", "example.com/app/main.Gone", "example.com/run.Main"],
		"modules": {"example.com/unlinked": {"init": "none", "pin": ["example.com/unlinked@v1"]}, "example.com/tree": {"init": "none", "pin": ["example.com/tree@v1"]}, "example.com/lone": {"init": "none"}},
		"packages": {
			"example.com/gone": {"default": "none"},
			"example.com/none/...": {"default": "none"},
			"example.com/run": {"default": "none", "functions": {"Start": "spawn:1", "Stop": "none", "Cmd.Dir": "spawn:field", "Cmd": "spawn:0", "Count": "program:arg0", "Start2": "program:arg0", "Self": "program:self", "Find": "program:lookup"}},
			"example.com/tree/...": {"default": "none", "functions": {"Close": "none", "Open": "calls:example.com/tree/a.Open"}},
			"example.com/tree/a": {"read": {"Open": "typeargs:constraint", "Node": "none", "Set": "typeargs:constraint"}},
			"example.com/lone/x": {"default": "none", "read": {"F": "none"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	err = class.Validate(suite)
	if err == nil {
		t.Fatal("Validate accepted dead entries")
	}
	for _, want := range []string{
		"cli example.com/gone.Main: the suite loads no package example.com/gone; delete the entry",
		"cli example.com/app/main.Gone: example.com/app/main declares no function Gone; delete the entry",
		"cli example.com/run.Main: example.com/run is not a package of the suite's modules, whose code alone a walk reads; delete the entry",
		"example.com/run.Count: program:arg0 names a []string variable",
		"example.com/run.Start2: no package it names declares Start2; delete the entry",
		"example.com/run.Self: Self's first result is not a string, so it names no program",
		"example.com/run.Find: Find's first parameter is not a string, so it looks no program up",
		"example.com/gone: the suite loads no package it names; delete the entry",
		"example.com/none/...: the suite loads no package it names; delete the entry",
		"example.com/run.Start: spawn argument is not a parameter of Start",
		"example.com/run.Stop: no package it names declares Stop; delete the entry",
		"example.com/run.Cmd.Dir: no package it names declares Cmd.Dir; delete the entry",
		"example.com/run.Cmd: a type is neither a spawner nor a program's source",
		"example.com/tree/....Close: no package it names declares Close; delete the entry",
		"example.com/tree/...: calls:example.com/tree/a.Open names no interface the suite loads",
		"module example.com/tree: pinned at example.com/tree@v1, and a measured build compiles it from ./tree@h1:x, example.com/tree@v2; re-read there the source of each function read in its packages, and update its pin",
		"module example.com/lone: its packages hold functions read in their source, and it has no pin naming the source they were read in",
		"module example.com/unlinked: a pin, and no package of it holds a function read in its source; delete the pin",
		"example.com/tree/a.Open: typeargs:constraint classifies a generic, and Open is none",
		"example.com/tree/a.Node: read names each function and method it read, and Node is a type",
		"example.com/tree/a.Set: read names each function and method it read, and Set is a type",
		"module example.com/unlinked: no measured build links a package of it; delete the entry",
		"module example.com/linked: a measured build links its packages, and the classification has no entry for it",
	} {
		if !strings.Contains(err.Error(), "\n  "+want) {
			t.Errorf("Validate error lacks %q:\n%v", want, err)
		}
	}
}

func TestTheEmbeddedClassificationParses(t *testing.T) {
	if _, err := parseClassification(classificationData); err != nil {
		t.Fatal(err)
	}
}

func TestAPinOfADirectoryModuleRefusesOnceAFileInItChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(shapeDir, "nested"))); err != nil {
		t.Fatal(err)
	}
	m := &listedModule{Path: "example.com/nested", Replace: &struct{ Path, Version, Dir string }{Path: "./nested", Dir: dir}}
	read, err := m.source()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(read, "./nested@h1:") {
		t.Fatalf("source = %q, want ./nested@ the hash of its content", read)
	}
	class, err := parseClassification([]byte(`{
		"modules": {"example.com/nested": {"init": "none", "pin": ["` + read + `"]}},
		"packages": {"example.com/nested/strs": {"default": "none", "read": {"Join": "none"}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{"example.com/nested/strs": "package strs; func Join() {}"}
	if err := class.Validate(suiteFrom(t, map[string]map[string]bool{"example.com/nested": {read: true}}, sources)); err != nil {
		t.Fatalf("Validate at the source read = %v, want nil", err)
	}
	f := filepath.Join(dir, "strs", "strs.go")
	src, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, append(src, "\n// changed\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := m.source()
	if err != nil {
		t.Fatal(err)
	}
	err = class.Validate(suiteFrom(t, map[string]map[string]bool{"example.com/nested": {changed: true}}, sources))
	want := "module example.com/nested: pinned at " + read + ", and a measured build compiles it from " + changed + "; re-read there the source of each function read in its packages, and update its pin"
	if err == nil || !strings.Contains(err.Error(), "\n  "+want) {
		t.Errorf("Validate after a change = %v\nwant it to say %s", err, want)
	}
}
