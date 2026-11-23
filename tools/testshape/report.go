package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// row is one line of the table: a package, or the total.
type row struct {
	name     string
	tests    int
	count    [numLayers]int
	wall     [numLayers]time.Duration
	touched  [numTouches]int
	testMain string
}

func (r *row) add(t testShape) {
	l := layerOf(t.touches)
	r.tests++
	r.count[l]++
	r.wall[l] += t.Elapsed
	for i, w := range t.touches {
		if w != nil {
			r.touched[i]++
		}
	}
}

// unitShare is the unit layer's share of the row's tests by count.
func (r row) unitShare() string {
	if r.tests == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(r.count[layerUnit])/float64(r.tests))
}

// relative is path with the module path left off; the module's root package
// is its own path.
func relative(module, path string) string {
	if rest, ok := strings.CutPrefix(path, module+"/"); ok {
		return rest
	}
	return path
}

// names is the touched touches' names, comma-separated; "-" for none.
func (ts touches) names() string {
	var ns []string
	for i, w := range ts {
		if w != nil {
			ns = append(ns, touch(i).String())
		}
	}
	if len(ns) == 0 {
		return "-"
	}
	return strings.Join(ns, ",")
}

// printTable renders one row per package, in the stream's order, and a TOTAL
// row: the count and wall time of top-level tests at each layer, the unit
// share by count, how many tests have each touch, and the package's
// TestMain's touches ("-" when it has none, "no TestMain" when there is
// none).
func printTable(w io.Writer, module string, shapes []packageShape) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{"package", "tests"}
	for _, l := range layerNames {
		header = append(header, l, l+" time")
	}
	header = append(header, "unit share")
	header = append(header, touchNames[:]...)
	header = append(header, "TestMain")
	fmt.Fprintln(tw, strings.Join(header, "\t")+"\t")
	total := row{name: "TOTAL"}
	for _, ps := range shapes {
		r := row{name: relative(module, ps.importPath), testMain: "no TestMain"}
		if ps.testMain != nil {
			r.testMain = ps.testMain.names()
		}
		for _, t := range ps.tests {
			r.add(t)
			total.add(t)
		}
		writeRow(tw, r)
	}
	writeRow(tw, total)
	tw.Flush()
}

func writeRow(w io.Writer, r row) {
	cells := []string{r.name, fmt.Sprint(r.tests)}
	for l := range numLayers {
		cells = append(cells, fmt.Sprint(r.count[l]), fmt.Sprintf("%.1fs", r.wall[l].Seconds()))
	}
	cells = append(cells, r.unitShare())
	for _, n := range r.touched {
		cells = append(cells, fmt.Sprint(n))
	}
	cells = append(cells, r.testMain)
	fmt.Fprintln(w, strings.Join(cells, "\t")+"\t")
}

// printTests lists every test: its package, name, layer, time and touches,
// then one witness chain per touch, the calls that reach it.
func printTests(w io.Writer, module string, shapes []packageShape) {
	for _, ps := range shapes {
		pkg := relative(module, ps.importPath)
		for _, t := range ps.tests {
			verdict := ""
			if t.Failed {
				verdict = "  [failed]"
			}
			fmt.Fprintf(w, "%s %s  %s  %.2fs  %s%s\n", pkg, t.Name, layerOf(t.touches), t.Elapsed.Seconds(), t.touches.names(), verdict)
			for i, chain := range t.touches {
				if chain != nil {
					fmt.Fprintf(w, "    %s: %s\n", touch(i), strings.Join(chain, " → "))
				}
			}
		}
	}
}
