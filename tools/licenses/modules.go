package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Module is one Go module in scope for license accounting: the coordinate
// go.mod declares, and the directory whose files answer for it.
//
// Those two are not always the same module, which is why ReplacedBy exists.
// Under a `replace` directive the go tool reports the ORIGINAL path and
// version alongside the REPLACEMENT's directory — verified against this
// repo's own go.mod, where `github.com/dolthub/dolt/go@v0.40.5-...62975ef`
// resolves to a directory belonging to github.com/brandon-fryslie/dolt/go at
// an entirely different version. A record that carried only Path, Version, and
// Dir would therefore state a license for a coordinate whose source it never
// opened, silently, with nothing in the output hinting that a substitution
// happened. ReplacedBy is empty for the ordinary case and names the substitute
// otherwise, so the discrepancy is a value the report can print rather than a
// fact only the go tool knows. [FRAMING:representation] the map says which
// territory it was drawn from.
type Module struct {
	Path       string
	Version    string
	Dir        string
	ReplacedBy string
}

// IsReplaced reports whether this module's source comes from somewhere other
// than the coordinate it is named by.
func (m Module) IsReplaced() bool { return m.ReplacedBy != "" }

// linkedModuleTemplate emits one tab-separated line per package `go list
// -deps` resolves, but only for packages that belong to a module: standard
// library packages have no .Module and are skipped, and the main module
// itself (Module.Main) is skipped because the binary's own code isn't a
// third-party dependency it must attribute.
const linkedModuleTemplate = `{{if and .Module (not .Module.Main)}}{{.Module.Path}}` + "\t" + `{{.Module.Version}}` + "\t" + `{{.Module.Dir}}` + "\t" + `{{if .Module.Replace}}{{.Module.Replace.Path}}{{if .Module.Replace.Version}}@{{.Module.Replace.Version}}{{end}}{{end}}` + "\n{{end}}"

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
		if len(parts) != 4 {
			return nil, fmt.Errorf("malformed go list output line (want 4 tab-separated fields): %q", line)
		}
		m := Module{Path: parts[0], Version: parts[1], Dir: parts[2], ReplacedBy: parts[3]}
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
