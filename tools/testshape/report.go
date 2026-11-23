package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// row is one package's shape, or the total's.
type row struct {
	name    string
	tests   int
	count   [layerCount]int
	elapsed [layerCount]time.Duration
	touched [touchCount]int // tests that reach each touch
	skipped int
}

func (r *row) add(s testShape) {
	r.tests++
	r.count[s.layer]++
	r.elapsed[s.layer] += s.Elapsed
	for t := touch(0); t < touchCount; t++ {
		if s.touches.has(t) {
			r.touched[t]++
		}
	}
	if s.Verdict == "skip" {
		r.skipped++
	}
}

// relPath is a package's path as the reports print it: relative to the
// module root.
func relPath(modPath, importPath string) string {
	if importPath == modPath {
		return "."
	}
	return strings.TrimPrefix(importPath, modPath+"/")
}

// printTable renders one row per package, in path order, then the total.
func printTable(w io.Writer, modPath string, shapes []testShape) {
	byPkg := map[string]*row{}
	total := &row{name: "TOTAL"}
	for _, s := range shapes {
		name := relPath(modPath, s.Package)
		r := byPkg[name]
		if r == nil {
			r = &row{name: name}
			byPkg[name] = r
		}
		r.add(s)
		total.add(s)
	}
	names := make([]string, 0, len(byPkg))
	for name := range byPkg {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(w, "testshape: top-level tests per layer, by what each reaches (%s)\n", modPath)
	header := []string{"package", "tests"}
	for l := layer(0); l < layerCount; l++ {
		header = append(header, l.String(), "time")
	}
	header = append(header, "unit%")
	for t := touch(0); t < touchCount; t++ {
		header = append(header, t.String())
	}
	header = append(header, "skip")
	lines := [][]string{header}
	for _, name := range names {
		lines = append(lines, rowCells(byPkg[name]))
	}
	writeAligned(w, append(lines, rowCells(total)))
}

// writeAligned prints lines as columns two spaces apart: the first, a name,
// flush left, the rest, numbers, flush right.
func writeAligned(w io.Writer, lines [][]string) {
	widths := make([]int, len(lines[0]))
	for _, l := range lines {
		for i, c := range l {
			widths[i] = max(widths[i], len(c))
		}
	}
	for _, l := range lines {
		var b strings.Builder
		for i, c := range l {
			if i == 0 {
				fmt.Fprintf(&b, "%-*s", widths[i], c)
				continue
			}
			fmt.Fprintf(&b, "  %*s", widths[i], c)
		}
		fmt.Fprintln(w, b.String())
	}
}

func rowCells(r *row) []string {
	cells := []string{r.name, fmt.Sprint(r.tests)}
	for l := layer(0); l < layerCount; l++ {
		cells = append(cells, fmt.Sprint(r.count[l]), fmt.Sprintf("%.1fs", r.elapsed[l].Seconds()))
	}
	cells = append(cells, fmt.Sprintf("%.0f%%", 100*float64(r.count[unit])/float64(max(r.tests, 1))))
	for t := touch(0); t < touchCount; t++ {
		cells = append(cells, fmt.Sprint(r.touched[t]))
	}
	return append(cells, fmt.Sprint(r.skipped))
}

// printTests lists every test, by package then name, with its layer, time,
// verdict and touches, and under it, for each touch, the chain of calls that
// reached it.
func printTests(w io.Writer, modPath string, shapes []testShape) {
	sorted := append([]testShape(nil), shapes...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Package != sorted[j].Package {
			return sorted[i].Package < sorted[j].Package
		}
		return sorted[i].Name < sorted[j].Name
	})
	for _, s := range sorted {
		fmt.Fprintf(w, "%-11s %8.2fs  %-10s  %s  %s  [%s]\n",
			s.layer, s.Elapsed.Seconds(), s.Verdict, relPath(modPath, s.Package), s.Name, s.touches)
		for t := touch(0); t < touchCount; t++ {
			if s.touches.has(t) {
				fmt.Fprintf(w, "    %s: %s\n", t, strings.Join(s.witness[t], " > "))
			}
		}
	}
	fmt.Fprintln(w)
}
