package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// row is one line of the table: a package, or the total.
type row struct {
	name     string
	tests    int
	skipped  int
	count    [numLayers]int
	elapsed  [numLayers]time.Duration
	touched  [numTouches]int
	init     string
	testMain string
}

func (r *row) add(t testShape) {
	if t.Verdict == gotestjson.Skip {
		r.skipped++
		return
	}
	l := layerOf(t.touches)
	r.tests++
	r.count[l]++
	r.elapsed[l] += t.Elapsed
	for i, w := range t.touches {
		if w != nil {
			r.touched[i]++
		}
	}
}

// unitShare is the unit layer's share of the row's layered tests by count.
func (r row) unitShare() string {
	if r.tests == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(r.count[layerUnit])/float64(r.tests))
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
// row: the count of layered top-level tests and of skipped ones, each
// layer's count and its tests' elapsed summed, the unit share by count, how
// many tests have each touch, and the touches of the package's initializer
// and of its TestMain ("no TestMain" when there is none).
func printTable(w io.Writer, module string, shapes []packageShape) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{"package", "tests", "skipped"}
	for _, l := range layerNames {
		header = append(header, l, l+" elapsed sum")
	}
	header = append(header, "unit share")
	header = append(header, touchNames[:]...)
	header = append(header, "init", "TestMain")
	fmt.Fprintln(tw, strings.Join(header, "\t")+"\t")
	total := row{name: "TOTAL"}
	for _, ps := range shapes {
		r := row{name: short(module, ps.importPath), init: ps.init.names(), testMain: "no TestMain"}
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
	cells := []string{r.name, fmt.Sprint(r.tests), fmt.Sprint(r.skipped)}
	for l := range numLayers {
		cells = append(cells, fmt.Sprint(r.count[l]), fmt.Sprintf("%.1fs", r.elapsed[l].Seconds()))
	}
	cells = append(cells, r.unitShare())
	for _, n := range r.touched {
		cells = append(cells, fmt.Sprint(n))
	}
	cells = append(cells, r.init, r.testMain)
	fmt.Fprintln(w, strings.Join(cells, "\t")+"\t")
}

// printTests lists every test: its package, name, layer (or that it was
// skipped), elapsed and touches, then one witness chain per touch.
func printTests(w io.Writer, module string, shapes []packageShape) {
	for _, ps := range shapes {
		pkg := short(module, ps.importPath)
		for _, t := range ps.tests {
			if t.Verdict == gotestjson.Skip {
				fmt.Fprintf(w, "%s %s  skipped  %.2fs\n", pkg, t.Name, t.Elapsed.Seconds())
				continue
			}
			mark := ""
			if t.Verdict == gotestjson.Fail {
				mark = "  [failed]"
			}
			fmt.Fprintf(w, "%s %s  %s  %.2fs  %s%s\n", pkg, t.Name, layerOf(t.touches), t.Elapsed.Seconds(), t.touches.names(), mark)
			for i, wit := range t.touches {
				if wit != nil {
					fmt.Fprintf(w, "    %s: %s\n", touch(i), wit.render(module))
				}
			}
		}
	}
}
