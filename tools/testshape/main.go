// testshape measures the test suite's shape per layer: for every top-level
// test a `go test -json` stream ran (or declared and did not run), which
// layer it sits at, decided by what its code can reach outside the process,
// and how long the tests at each layer took.
//
//	go test -short -json ./... > shape.json
//	go -C internal/vendor/dolthub-driver test -short -json ./... >> shape.json
//	go run ./tools/testshape [-tests] [-tags list] < shape.json
//
// Each module's stream is its own run, so one module's failing test does
// not keep the other's results out of the measure. -tags names the build
// tags the measured runs passed, which the stream does not carry.
//
// It prints a table, one row per package: the tests it declares and the
// tests that ran, the count of declared tests at each layer, the elapsed of
// those that ran summed per layer, the unit share, per touch the tests that
// can reach it, and the touches of the package's setup; then the count of
// outside generic instances whose bodies could not be read. -tests adds a
// listing of every test's layer, its touches, one witness chain per touch,
// and each type argument an outside generic instance withheld.
//
// THE INVARIANT. The unit share is what the suite's layering is judged
// by, so a measure that overstates it certifies work that was never done.
// Every approximation this tool makes errs away from unit: an imprecision
// may add a touch a test does not have, or move a test between integration
// and end-to-end; it never removes a touch. A
// touch is charged where module code, or a classified outside function, can
// call it: charging every method of every value that reaches an interface
// would charge nearly every test with what testing.T can do, since nearly
// every test hands t to a helper, and the measure would stop telling tests
// apart.
//
// TOUCHES. A store on disk, git, a CLI run, any other process, the network,
// the filesystem, and the real clock. A test that can reach none is unit; one
// that can reach a CLI run is end-to-end; any other is integration. A test's
// touches include its binary's setup's — the initialization of every package
// in the binary, and its TestMain — since `go test -run` runs the setup for
// every test it runs; the witness of such a touch starts at the setup.
//
// REACH. What a test can run is decided without following values to where
// they are stored: stores into package variables, pointers, fields, maps and
// initializer contents have unbounded shapes, and an analysis that follows
// some of them silently drops a touch on every other one. Instead reach is
// rapid type analysis, over the address-taken functions and runtime types
// that exist (see rta.go):
//
//   - A binary's setup is walked first, from the initializer of every
//     package the binary links, the module's and every outside one, and
//     from its TestMain; each test is walked over it, so the
//     address-taken functions and runtime types of both meet every dynamic
//     and interface call of both.
//   - A static call reaches its callee. A function taken as a value is
//     address-taken, and taking it runs nothing: a dynamic call reaches every
//     address-taken function of its signature. A function value converted to
//     an interface is a runtime type as well.
//   - One rule holds at every callee the walk does not follow: an outside
//     function or method, and the target of every dynamic call, which may
//     be one. It can call back every address-taken function of a signature
//     its receiver or parameters can carry, decided by their types alone: a
//     function type in their structure (fields, elements, pointees), or a
//     method of an interface there; and, handed an interface (`any`
//     included), every function converted to one, which reflection can
//     call. A function stored in a field of a struct handed to outside code
//     (`&cobra.Command{RunE: f}`) is reached as surely as one passed
//     directly; so is a range-over-func loop's body, which the iterator is
//     handed, and a fuzz function handed to f.Fuzz. A value with no use
//     creates nothing, so `var _ I = (*T)(nil)` makes no method reachable.
//   - Converting a module type to an interface makes reachable every method
//     of it and of every module type reachable from its structure (fields,
//     elements, pointees), since fmt and encoding/json call them by
//     reflection; an interface in that structure stands for every runtime
//     type implementing it.
//   - An outside type, converted or found in such a structure, contributes
//     only what module code can call on it: the methods implementing a
//     module interface method it is converted to, and each module interface
//     method module code calls (`*http.Client` behind a module interface
//     reaches its Do).
//   - A call of a module interface method reaches that method of every
//     runtime type implementing the interface. A call of an outside
//     interface method (`testing.TB`'s TempDir, `io.Writer`'s Write) is a
//     call of an outside method, charged by its classification; the runtime
//     module types implementing it are reached as well.
//   - What an outside generic's instance does with its type arguments is
//     read from the generic's source, the files the build compiles (see
//     generic.go). A value of a type parameter leaves a body only converted
//     to an interface, as a type mentioning the parameter, or handed to
//     another generic instantiated over one, whose body is read in turn. A
//     type a body converts reaches exactly what module code converting it
//     reaches; a constraint method a body calls (`Join[T fmt.Stringer]`
//     calls T's String) is reached on the type argument, an interface call
//     where the type argument is an interface; a type argument no body
//     converts or calls a method of reaches nothing, and is listed as
//     withheld. A method's type arguments are its receiver type's, a bound
//     method value's included. A body that cannot be read (no source that
//     type-checks, no Go body, a type parameter's value taken as an
//     unsafe.Pointer, a type declared in the body) converts every type
//     argument; the witness says why, and the output counts each such
//     instance with why. Nothing about a generic is recorded in the
//     classification.
//   - Code outside the module is never walked. Every outside function module
//     code reaches is classified in classification.json: the touches it can
//     make, none,
//     dispatching — it calls back what was registered with it or handed to
//     it (a command tree's Execute, a server, a database driver), so a call
//     reaches every address-taken function and every method of every
//     runtime module type — or the interfaces it calls back on what it is
//     handed (fmt calls a fmt.Stringer's String), each outside type's method
//     charged by its own classification.
//   - An outside package's initializer is classified by its module's entry,
//     which every module the measured builds link a package of must have
//     ("std" for the standard library), so a dependency added later cannot
//     bring an initializer in unread; a package's "init" entry overrides
//     its module's.
//   - A spawn's program is the one value followed: through parameters (at the
//     static calls in the test's reach) and closure bindings, to a constant
//     or to what the classification says names a program (os.Executable()
//     and os.Args[0] the test binary, exec.LookPath what its argument
//     names). A parameter of a function reached other than by a static call
//     may hold anything, so it is unresolved beside its static calls'
//     arguments. A program is git when it names git; a CLI run when it is
//     the test binary and the binary's setup reaches the CLI; another
//     process otherwise. An unresolved program may be any of them: another
//     process, and a CLI run where the setup reaches the CLI. The witness
//     says which program, or why it is unresolved.
//
// REFUSALS. A measure that measured nothing, or would mismeasure, names
// each cause and prints no table:
//
//   - The stream: a package that failed to build, failed outside any test
//     (in its initialization, or in TestMain before or after the tests), or
//     ended with a test still running; a stream that ended inside a package;
//     a package or test the stream ran twice; a test the stream ran that its
//     binary does not declare; a package of no module of the suite (every
//     go.mod outside testdata, as ./... walks); a package whose test binary
//     declares tests and that the stream has no result for.
//   - The classification: an entry naming a package or member the suite does
//     not load, a module no measured build links, or a CLI entry point
//     outside the suite's modules (delete it); a module a measured build
//     links with no entry; a class its member cannot have (a spawn argument
//     its function lacks, a program's source whose result is not a string).
//     These stop the run before any measuring.
//   - The analysis: an outside function module code reaches with no
//     classification, with its first caller; a module function with no Go
//     body to walk; anything the analysis's own model rules out, a bug in
//     the tool (a type the source copy of an outside package declares that
//     its export data does not).
//
// A test counts as declared when go test would run it: a Test or Fuzz
// function, or an Example with an output comment. A name both a package and
// its external test package declare is one test, reaching what both reach.
//
// EXIT CODES. 0: measured. 1: refused, as above. 2: a flag it does not
// take. 3: the stream is malformed, under gotestjson's rule. 4: the suite,
// or the classification file, did not load.
//
// THE STREAM is read through tools/internal/gotestjson, the reader and clock
// tools/testbudget uses, so the two never report different durations for one
// test; the reader's replay of go test's output goes to stderr. Every
// declared top-level test counts in its layer, since its layer comes from
// its static reach; a test the run skipped or did not select contributes no
// duration and is listed as not run.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"go/types"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

//go:embed classification.json
var classificationData []byte

const (
	exitMeasured  = 0
	exitRefused   = 1
	exitUsage     = 2
	exitMalformed = 3
	exitLoad      = 4
)

func main() {
	os.Exit(run(classificationData, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(classData []byte, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("testshape", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listTests := fs.Bool("tests", false, "list each test's layer, touches, and a witness per touch")
	tags := fs.String("tags", "", "the -tags the measured runs passed to go test")
	root := fs.String("root", ".", "the repository whose suite the stream measured")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	var buildFlags []string
	if *tags != "" {
		buildFlags = []string{"-tags=" + *tags}
	}
	class, err := parseClassification(classData)
	if err != nil {
		fmt.Fprintf(stderr, "testshape: %v\n", err)
		return exitLoad
	}
	stream, err := gotestjson.Read(stdin, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "testshape: %v\n", err)
		return exitMalformed
	}
	s, refusals, err := measureSuite(*root, buildFlags, class, stream)
	if err != nil {
		fmt.Fprintf(stderr, "testshape: %v\n", err)
		return exitLoad
	}
	if len(refusals) > 0 {
		for _, r := range refusals {
			fmt.Fprintf(stderr, "testshape: refused: %s\n", r)
		}
		return exitRefused
	}
	var rows []row
	for _, m := range s.packages {
		rows = append(rows, rowOf(m.measuredPackage, m.module.Path))
	}
	writeTable(stdout, rows)
	writeUnread(stdout, s.unread)
	if *listTests {
		writeListing(stdout, s.packages)
	}
	return exitMeasured
}

// loadedSuite is what the suite's loads hold, which the classification is
// checked against.
type loadedSuite struct {
	packages map[string]*types.Package // every package of every load, by path
	moduleOf map[string]string         // each of those packages' module
	suite    map[string]bool           // the suite's modules
	linked   map[string]bool           // each module a load links packages of beyond its own
}

func suiteOf(loads []*loaded) loadedSuite {
	s := loadedSuite{packages: map[string]*types.Package{}, moduleOf: map[string]string{}, suite: map[string]bool{}, linked: map[string]bool{}}
	for _, l := range loads {
		s.suite[l.Module.Path] = true
		for pkg, mod := range l.ModuleOf {
			s.moduleOf[pkg] = mod
			if mod != l.Module.Path {
				s.linked[mod] = true
			}
		}
		for _, sp := range l.Prog.AllPackages() {
			if s.packages[sp.Pkg.Path()] == nil {
				s.packages[sp.Pkg.Path()] = sp.Pkg
			}
		}
	}
	return s
}

type modulePackage struct {
	measuredPackage
	module module
}

// moduleUnread is an outside generic instance one module's analysis read
// whose body could not be read.
type moduleUnread struct {
	unreadInstance
	module module
}

// suiteShape is what the suite's measure found: the measured packages, in
// import-path order, and every outside generic instance whose body could
// not be read.
type suiteShape struct {
	packages []modulePackage
	unread   []moduleUnread
}

// measureSuite measures every package of stream against the suite under
// root, returning what it found and every refusal. err is for a failure to
// load the suite.
func measureSuite(root string, buildFlags []string, class *Classification, stream gotestjson.Run) (suiteShape, []string, error) {
	mods, err := suiteModules(root)
	if err != nil {
		return suiteShape{}, nil, err
	}
	loads := make([]*loaded, len(mods))
	for i, m := range mods {
		l, err := loadModule(m, buildFlags)
		if err != nil {
			return suiteShape{}, nil, err
		}
		loads[i] = l
	}
	s := suiteOf(loads)
	refusals := append(streamRefusals(stream), strayRefusals(s.moduleOf, s.suite, stream)...)
	if err := class.Validate(s); err != nil {
		return suiteShape{}, append(refusals, err.Error()), nil
	}

	inStream := map[string]gotestjson.Package{}
	for _, p := range stream.Packages {
		inStream[p.ImportPath] = p
	}
	var out suiteShape
	unread := map[string]bool{}
	for _, l := range loads {
		owned := func(path string) bool { return s.moduleOf[path] == l.Module.Path }
		refusals = append(refusals, absentRefusals(l.Binaries, inStream, owned)...)
		a := newAnalyzer(l, class)
		for _, path := range sortedKeys(l.Binaries) {
			p, ok := inStream[path]
			if !ok {
				continue
			}
			mp, rs := join(p, a.measure(l.Binaries[path]))
			refusals = append(refusals, rs...)
			out.packages = append(out.packages, modulePackage{mp, l.Module})
		}
		for _, u := range a.unreadInstances() {
			// Each module's analysis reads over its own program, so one
			// instance two modules read is one by its origin and the full
			// paths of its type arguments.
			if id := u.in.String(); !unread[id] {
				unread[id] = true
				out.unread = append(out.unread, moduleUnread{u, l.Module})
			}
		}
		refusals = append(refusals, analysisRefusals(a)...)
	}
	slices.SortFunc(out.packages, func(a, b modulePackage) int { return strings.Compare(a.ImportPath, b.ImportPath) })
	return out, refusals, nil
}
