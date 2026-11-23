// actionnames-check refuses an ActionName formatted into a message as its
// persisted event encoding (see internal/actionnames), in every non-test file
// of this module. CI's lint job runs it on every PR.
//
// It is a command, not a test, for the reason fsnames-check is: reading types
// needs the export data of every dependency, Dolt's cgo graph included, and
// compiled inside go test's run that build contends with every package's
// tests.
//
// It loads the module once per GOOS lit ships for, so the files only one OS
// builds (internal/cli's _windows.go files, internal/dbsnapshot's
// _darwin.go) are read wherever it runs. A load for another OS builds with
// cgo off, so Dolt's cgo-only dependencies fail to type-check there; the
// check reads only the module's own packages, and refuses any of those that
// did not type-check, since the rule cannot see into one.
//
// It exits 0 when nothing is found and 1 otherwise, naming each site on
// stderr.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"

	"golang.org/x/tools/go/packages"

	"github.com/promptctl/links-issue-tracker/internal/actionnames"
)

// targets are the GOOSes lit ships for.
var targets = []string{"linux", "darwin", "windows"}

func main() {
	os.Exit(run(os.Stderr))
}

func run(stderr io.Writer) int {
	var found []string
	for _, goos := range targets {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
			Env:  append(os.Environ(), "GOOS="+goos),
		}
		pkgs, err := packages.Load(cfg, "./...")
		if err != nil {
			fmt.Fprintf(stderr, "actionnames-check: GOOS=%s: %v\n", goos, err)
			return 1
		}
		// PrintErrors reads only these packages, the module's own: a
		// dependency's own errors leave the types this check reads whole.
		if packages.PrintErrors(pkgs) > 0 {
			fmt.Fprintf(stderr, "actionnames-check: GOOS=%s: packages of the module failed to type-check; the rule cannot read them\n", goos)
			return 1
		}
		files, allowed := 0, 0
		for _, pkg := range pkgs {
			files += len(pkg.Syntax)
			f, a := actionnames.Check(pkg.Fset, pkg.Syntax, pkg.TypesInfo)
			for _, x := range f {
				found = append(found, x.String())
			}
			allowed += a
		}
		// The module formats verbs; a load in which the check saw none is not
		// reading the module. [LAW:no-silent-failure]
		if files == 0 || allowed == 0 {
			fmt.Fprintf(stderr, "actionnames-check: GOOS=%s: read %d files and no ActionName formatted the allowed way; run it from the repository root\n", goos, files)
			return 1
		}
	}
	// A file every GOOS builds is read once per GOOS.
	slices.Sort(found)
	found = slices.Compact(found)
	for _, f := range found {
		fmt.Fprintln(stderr, f)
	}
	if len(found) > 0 {
		fmt.Fprintln(stderr, "an action's persisted event encoding reaches a message a caller reads; use x.Verb() for the word the caller typed, or write string(x) to say the persisted encoding is meant")
		return 1
	}
	return 0
}
