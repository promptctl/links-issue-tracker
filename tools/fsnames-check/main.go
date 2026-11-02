// fsnames-check refuses an io/fs name that can hold the host's separator, in
// every package of this module and of the vendored driver module, test files
// included. CI's lint job runs it on every PR; the nightly Windows lane runs
// it again so the _windows.go files, which only build there, are read too.
//
// It is a command, not a test, because reading types needs the export data of
// every dependency, Dolt's cgo graph included. Compiled inside go test's run,
// that build contends with every package's tests and inflates the whole
// tools/testbudget table; the lint job has already built it.
//
// It exits 0 when no read is found and 1 otherwise, naming each read on stderr.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"

	"golang.org/x/tools/go/packages"

	"github.com/promptctl/links-issue-tracker/internal/fsnames"
)

func main() {
	os.Exit(run(os.Stderr))
}

func run(stderr io.Writer) int {
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...", "github.com/dolthub/driver/...")
	if err != nil {
		fmt.Fprintln(stderr, "fsnames-check:", err)
		return 1
	}
	if packages.PrintErrors(pkgs) > 0 {
		fmt.Fprintln(stderr, "fsnames-check: packages failed to load; their types cannot be read")
		return 1
	}
	var found []string
	files := 0
	for _, pkg := range pkgs {
		files += len(pkg.Syntax)
		for _, f := range fsnames.Check(pkg.Fset, pkg.Syntax, pkg.TypesInfo) {
			found = append(found, f.String())
		}
	}
	if files == 0 {
		fmt.Fprintln(stderr, "fsnames-check: read no files; run it from the repository root")
		return 1
	}
	// A package loads once alone and once with its tests, so a file can be
	// read twice.
	slices.Sort(found)
	found = slices.Compact(found)
	for _, f := range found {
		fmt.Fprintln(stderr, f)
	}
	if len(found) > 0 {
		fmt.Fprintln(stderr, "an io/fs name is slash-separated on every OS, so a name built with filepath fails every lookup on Windows; build it with path (or convert a host path with filepath.ToSlash)")
		return 1
	}
	return 0
}
