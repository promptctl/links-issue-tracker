// actionnames-check refuses an ActionName formatted into a message as its
// persisted event encoding (see internal/actionnames), in every non-test file
// of this module. CI's lint job runs it on every PR.
//
// It is a command, not a test, for the reason fsnames-check is: reading types
// needs the export data of every dependency, Dolt's cgo graph included, and
// compiled inside go test's run that build contends with every package's
// tests.
//
// It loads the module once per GOOS lit ships for, read from
// .goreleaser.yml, the one place that list is kept, so the files only one OS
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
	"path/filepath"
	"slices"

	"golang.org/x/tools/go/packages"
	"gopkg.in/yaml.v3"

	"github.com/promptctl/links-issue-tracker/internal/actionnames"
)

func main() {
	targets, err := shippedGOOS(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "actionnames-check: %v\n", err)
		os.Exit(1)
	}
	os.Exit(run(targets, ".", []string{"./..."}, os.Stderr))
}

// shippedGOOS is every GOOS a build in root's .goreleaser.yml targets.
func shippedGOOS(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yml"))
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Builds []struct {
			GOOS []string `yaml:"goos"`
		} `yaml:"builds"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf(".goreleaser.yml: %w", err)
	}
	var targets []string
	for _, b := range cfg.Builds {
		targets = append(targets, b.GOOS...)
	}
	slices.Sort(targets)
	targets = slices.Compact(targets)
	if len(targets) == 0 {
		return nil, fmt.Errorf(".goreleaser.yml: no build names a goos")
	}
	return targets, nil
}

// run checks the packages patterns match, in the module at root, once per
// GOOS in targets, and returns the exit code.
func run(targets []string, root string, patterns []string, stderr io.Writer) int {
	var found []string
	for _, goos := range targets {
		cfg := &packages.Config{
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
			Dir:  root,
			Env:  append(os.Environ(), "GOOS="+goos),
		}
		pkgs, err := packages.Load(cfg, patterns...)
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
		var all []actionnames.Package
		for _, pkg := range pkgs {
			all = append(all, actionnames.Package{Files: pkg.Syntax, Info: pkg.TypesInfo})
		}
		wrappers := actionnames.Wrappers(all)
		files, allowed := 0, 0
		for _, pkg := range pkgs {
			files += len(pkg.Syntax)
			f, a := actionnames.Check(pkg.Fset, pkg.Syntax, pkg.TypesInfo, wrappers)
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
