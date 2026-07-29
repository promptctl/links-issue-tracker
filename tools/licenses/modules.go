package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Module is one Go module compiled into the target package's binary.
type Module struct {
	Path    string
	Version string
	Dir     string
}

// linkedModuleTemplate emits one tab-separated line per package `go list
// -deps` resolves, but only for packages that belong to a module: standard
// library packages have no .Module and are skipped, and the main module
// itself (Module.Main) is skipped because the binary's own code isn't a
// third-party dependency it must attribute.
const linkedModuleTemplate = `{{if and .Module (not .Module.Main)}}{{.Module.Path}}` + "\t" + `{{.Module.Version}}` + "\t" + `{{.Module.Dir}}` + "\n{{end}}"

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
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed go list output line (want 3 tab-separated fields): %q", line)
		}
		m := Module{Path: parts[0], Version: parts[1], Dir: parts[2]}
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
