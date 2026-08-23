package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// Module is one Go module in scope for license accounting: the coordinate
// go.mod declares, and the directory whose files answer for it.
//
// Those two are not always the same module, which is why Replacement exists.
// Under a `replace` directive the go tool reports the ORIGINAL path and
// version alongside the REPLACEMENT's directory — verified against this
// repo's own go.mod, where `github.com/dolthub/dolt/go@v0.40.5-...62975ef`
// resolves to a directory belonging to github.com/promptctl/dolt/go at
// an entirely different version. A record that carried only Path, Version, and
// Dir would therefore state a license for a coordinate whose source it never
// opened, silently, with nothing in the output hinting that a substitution
// happened. Replacement is the zero value for the ordinary case and names the
// substitute otherwise, so the discrepancy is a value the artifacts can render
// rather than a fact only the go tool knows. [FRAMING:representation] the map
// says which territory it was drawn from.
type Module struct {
	Path        string
	Version     string
	Dir         string
	Replacement Replacement
}

// IsReplaced reports whether this module's source comes from somewhere other
// than the coordinate it is named by.
func (m Module) IsReplaced() bool { return m.Replacement.Kind != NotReplaced }

// ReplacementKind discriminates the three ways a module's source can relate to
// the coordinate naming it. They are not three shades of one thing: each admits
// a different set of facts, and a renderer that cannot tell them apart will
// state something untrue about one of them. A module replacement has a
// resolvable coordinate — path AND version — that the SBOM can hand a reader to
// fetch and diff. A directory replacement has no coordinate at all; its source
// exists only inside this repository, so an SBOM that invented a purl for it
// would name something nobody else can resolve. [LAW:types-are-the-program]
type ReplacementKind int

const (
	// NotReplaced: the module's source is the coordinate that names it.
	NotReplaced ReplacementKind = iota
	// ReplacedByModule: the source came from a different published module
	// coordinate (`replace x => y v1.2.3`) — lit's forks of dolt and
	// go-mysql-server.
	ReplacedByModule
	// ReplacedByDirectory: the source came from a filesystem path
	// (`replace x => ./dir`) — lit's patched copy of dolthub/driver.
	ReplacedByDirectory
)

// Replacement is where a module's source actually came from. Path and Version
// are meaningful exactly as Kind says: empty for NotReplaced, a module path
// plus its version for ReplacedByModule, and a filesystem path with NO version
// for ReplacedByDirectory. parseReplacement is the only constructor, so no
// consumer has to re-derive which shape it is holding from whether a version
// happens to be present.
type Replacement struct {
	Kind    ReplacementKind
	Path    string
	Version string
}

// String renders the replacement the way a human reads a coordinate —
// "path@version" for a module, the bare path for a directory, empty for none.
// It is the one spelling of a substitution across the graph audit and the
// license report, so the two documents cannot describe the same fact
// differently. [LAW:one-source-of-truth]
//
// [LAW:dataflow-not-control-flow] the switch is the domain's own discriminator,
// and every kind is named. An unreplaced value renders as nothing because it HAS
// no source to name — not because its Path happens to be empty, which is an
// invariant of how it was built rather than one the type holds.
func (r Replacement) String() string {
	switch r.Kind {
	case NotReplaced:
		return ""
	case ReplacedByModule:
		return r.Path + "@" + r.Version
	case ReplacedByDirectory:
		return r.Path
	default:
		panic(fmt.Sprintf(unhandledReplacementKind, r.Kind, r.Path))
	}
}

// unhandledReplacementKind is what every renderer says about a kind nobody
// taught it. Go's switch has no exhaustiveness check and this repo runs no
// linter that supplies one (.golangci.yml enables depguard alone), so a fourth
// shape added to ReplacementKind would otherwise reach a `default` arm and
// render as NOT REPLACED — silently restoring, for the new shape, the very
// undisclosed substitution this package exists to prevent. That failure would
// surface as a shipped compliance artifact that is quietly wrong, which is the
// worst place for it.
//
// It is a panic rather than an error because these renderers have no failure
// arm to return through, and because the only way to reach it is to edit the
// enum: a build-time generator that dies loudly during a release is the correct
// outcome, and an unreachable-in-production branch is exactly what a panic is
// for. The message is a single const so the three renderers cannot describe the
// same condition differently. [LAW:no-silent-failure] [LAW:one-source-of-truth]
const unhandledReplacementKind = "licenses: ReplacementKind %d (path %q) reached a renderer that does not handle it; teach every renderer the new replacement shape before shipping it"

// parseReplacement turns `go list`'s two replacement columns into the shape
// they encode, refusing every combination the go tool does not produce.
// [LAW:parse-dont-validate] its output is a value that could not have existed
// before the check: downstream code switches on a Kind that was established
// here, and never re-asks whether an empty version means "a directory" or
// "a field somebody forgot to fill".
//
// The cross-check is the point, not ceremony. `go.mod` grammar permits a
// version-less replacement target only when it is a filesystem path, so
// path-shape and version-presence must agree. If they ever disagree — a go
// release changing the template's semantics, a module path arriving with no
// version — the silent outcome would be an SBOM pedigree stating "no published
// coordinate identifies this source" about a coordinate that is published, i.e.
// a false claim in a shipped compliance artifact. [LAW:no-silent-failure]
//
// Each arm proves its own shape with the go tool's own rule rather than by
// complement: modfile.IsDirectoryPath is the predicate the go command itself
// uses to decide that a replacement target is a directory, and module.CheckPath
// is the one that decides a string is a module coordinate. Asking both, instead
// of treating "not a module path" as sufficient evidence of a directory, is
// what makes a target that is NEITHER — the shape a future go release or a
// malformed template would produce — fail rather than get filed under whichever
// arm it fell through to.
func parseReplacement(path, version string) (Replacement, error) {
	// One call each, so the verdict an arm branches on and the reason its error
	// quotes cannot disagree. [LAW:one-source-of-truth]
	pathErr := module.CheckPath(path)
	isModulePath := pathErr == nil
	isDirectory := modfile.IsDirectoryPath(path)
	switch {
	case path == "" && version == "":
		return Replacement{}, nil
	case path == "":
		return Replacement{}, fmt.Errorf("replacement version %q with no replacement path", version)
	case version == "" && isModulePath:
		return Replacement{}, fmt.Errorf("replacement path %q is a module path but carries no version; go.mod permits a version-less target only for a filesystem path", path)
	case version == "" && !isDirectory:
		return Replacement{}, fmt.Errorf("replacement path %q carries no version but is neither a module path nor a filesystem path (go.mod requires a directory target to begin with ./, ../ or /): %w", path, pathErr)
	case version == "":
		return Replacement{Kind: ReplacedByDirectory, Path: path}, nil
	case isDirectory:
		return Replacement{}, fmt.Errorf("replacement path %q is a filesystem path but carries version %q; go.mod permits a version only on a module target", path, version)
	case !isModulePath:
		return Replacement{}, fmt.Errorf("replacement path %q carries version %q but is not a valid module path: %w", path, version, pathErr)
	default:
		return Replacement{Kind: ReplacedByModule, Path: path, Version: version}, nil
	}
}

// moduleFields is the record layout parseModuleList reads: the five
// tab-separated columns of one module, with dot already bound to a module. Both
// `go list` invocations in this package emit it — the linked-package scan below
// and the build-list scan in graph.go — so the column count and order have ONE
// spelling. They had two until the replacement's path and version were split
// apart.
//
// A diverging column COUNT would at least fail loudly, at parseModuleList's
// arity guard. The divergence worth fearing is a same-count REORDER, which no
// guard can see: swap two columns in one producer and a module Dir gets read
// out of the replacement column, so the scan reports "no license file found"
// for a directory that was never opened — a failure that names the wrong cause
// in the wrong place. One layout is what makes that unrepresentable rather than
// merely unlikely. [LAW:one-source-of-truth]
//
// The replacement's path and version are two SEPARATE fields rather than one
// joined with an "@". Joining them here would only force parseReplacement to
// split them apart again on a character that carries no guarantee, and it would
// make "no version" indistinguishable from "a version that failed to render".
// Two fields keep the go tool's own answer intact all the way to the parser.
// [FRAMING:representation]
const moduleFields = `{{.Path}}` + "\t" + `{{.Version}}` + "\t" + `{{.Dir}}` + "\t" +
	`{{if .Replace}}{{.Replace.Path}}{{end}}` + "\t" + `{{if .Replace}}{{.Replace.Version}}{{end}}` + "\n"

// linkedModuleTemplate emits one moduleFields record per package `go list
// -deps` resolves, but only for packages that belong to a module: standard
// library packages have no .Module and are skipped, and the main module
// itself (Module.Main) is skipped because the binary's own code isn't a
// third-party dependency it must attribute. `with` rebinds dot to the package's
// module, which is what lets the shared layout above be written once against a
// module rather than twice against two different shapes of dot.
const linkedModuleTemplate = `{{if and .Module (not .Module.Main)}}{{with .Module}}` + moduleFields + `{{end}}{{end}}`

// LinkedModules resolves the set of external modules actually compiled into
// pkg (e.g. "./cmd/lit") — the same set `go build` would link — via `go list
// -deps`. [LAW:effects-at-boundaries] this is the one place the tool shells
// out; parseModuleList below is pure and independently testable.
func LinkedModules(pkg string) ([]Module, error) {
	cmd := exec.Command("go", "list", "-deps", "-f", linkedModuleTemplate, pkg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list -deps %s: %w\n%s", pkg, err, stderr.String())
	}
	return parseModuleList(stdout.String())
}

// parseModuleList parses linkedModuleTemplate's output. A module typically
// contributes several linked subpackages (e.g. golang.org/x/sys shows up via
// many import paths), so the same module path can appear on many lines; the
// result is deduplicated to one entry per module path and sorted so the
// bundle and report have a fixed order regardless of `go list`'s (unspecified)
// emission order. [LAW:dataflow-not-control-flow] sorting always runs —
// determinism isn't conditional on the input.
func parseModuleList(output string) ([]Module, error) {
	seen := make(map[string]Module)
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 5 {
			return nil, fmt.Errorf("malformed go list output line (want 5 tab-separated fields): %q", line)
		}
		replacement, err := parseReplacement(parts[3], parts[4])
		if err != nil {
			return nil, fmt.Errorf("go list line %q: %w", line, err)
		}
		m := Module{Path: parts[0], Version: parts[1], Dir: parts[2], Replacement: replacement}
		if m.Path == "" || m.Dir == "" {
			return nil, fmt.Errorf("go list emitted an incomplete module record: %q", line)
		}
		seen[m.Path] = m
	}

	mods := make([]Module, 0, len(seen))
	for _, m := range seen {
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}
