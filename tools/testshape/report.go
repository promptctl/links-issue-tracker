package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// row is one package's line of the table.
type row struct {
	Name    string
	Ran     int // declared tests the stream ran
	Count   [numLayers]int
	Summed  [numLayers]time.Duration // elapsed of the tests that ran, summed
	Touched [numTouches]int          // tests reaching each touch
	Setup   Touches
}

func (r *row) add(o row) {
	r.Ran += o.Ran
	for l := range numLayers {
		r.Count[l] += o.Count[l]
		r.Summed[l] += o.Summed[l]
	}
	for t := range numTouches {
		r.Touched[t] += o.Touched[t]
	}
	r.Setup = r.Setup.Union(o.Setup)
}

func (r row) total() int { return r.Count[Unit] + r.Count[Integration] + r.Count[EndToEnd] }

// unitShare is the unit layer's share of the row's tests by count, the
// figure the suite's layering is judged by: overstating it certifies work
// that was never done.
func (r row) unitShare() string {
	if r.total() == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(r.Count[Unit])/float64(r.total()))
}

func rowOf(m measuredPackage, modPath string) row {
	r := row{Name: strings.TrimPrefix(m.ImportPath, modPath+"/"), Setup: m.Setup.Reached.set()}
	for _, t := range m.Tests {
		l := t.Layer()
		r.Count[l]++
		if t.Ran {
			r.Ran++
		}
		r.Summed[l] += t.Elapsed
		for touch := range t.Reached {
			r.Touched[touch]++
		}
	}
	return r
}

// writeTable renders one row per package and a TOTAL row.
func writeTable(w io.Writer, rows []row) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	header := []string{"package", "tests", "ran", "unit", "integ", "e2e", "unit%", "Σunit", "Σinteg", "Σe2e"}
	for t := range numTouches {
		header = append(header, t.String())
	}
	header = append(header, "setup")
	fmt.Fprintln(tw, strings.Join(header, "\t")+"\t")
	var total row
	total.Name = "TOTAL"
	for _, r := range rows {
		writeRow(tw, r)
		total.add(r)
	}
	writeRow(tw, total)
	tw.Flush()
	fmt.Fprintln(w, "\ntests: the top-level tests the package declares; ran: those the stream ran. unit/integ/e2e: declared tests")
	fmt.Fprintln(w, "at each layer. Σ: the elapsed of the tests that ran, summed, waits included,")
	fmt.Fprintln(w, "so a package's sums can exceed its wall time.")
	fmt.Fprintln(w, "touch columns: tests that can reach the touch. setup: touches of the package's initialization and TestMain.")
}

func writeRow(w io.Writer, r row) {
	cells := []string{
		r.Name, fmt.Sprint(r.total()), fmt.Sprint(r.Ran),
		fmt.Sprint(r.Count[Unit]), fmt.Sprint(r.Count[Integration]), fmt.Sprint(r.Count[EndToEnd]),
		r.unitShare(),
		seconds(r.Summed[Unit]), seconds(r.Summed[Integration]), seconds(r.Summed[EndToEnd]),
	}
	for t := range numTouches {
		cells = append(cells, fmt.Sprint(r.Touched[t]))
	}
	setup := "-"
	if r.Setup != 0 {
		setup = r.Setup.String()
	}
	cells = append(cells, setup)
	fmt.Fprintln(w, strings.Join(cells, "\t")+"\t")
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

// writeGenerics renders what the reading of outside generics decided
// beyond the table: the count of type parameters instances withheld, which
// -tests lists; and the count of instances whose bodies could not be read,
// each counted as converting every type argument, with why each could not
// be.
func writeGenerics(w io.Writer, unread []moduleUnread, withheld int) {
	var lines []string
	for _, u := range unread {
		lines = append(lines, u.text(namer{u.module}))
	}
	slices.Sort(lines)
	fmt.Fprintf(w, "\ntype parameters of outside generic instances withheld, reaching nothing of their type arguments: %d\n", withheld)
	fmt.Fprintf(w, "outside generic instances whose bodies could not be read, each counted as converting every type argument: %d\n", len(lines))
	for _, l := range lines {
		fmt.Fprintf(w, "  %s\n", l)
	}
}

// writeListing renders each package's setup touches and each test's layer,
// its touches, and the chain that reaches each; and, for the setup and each
// test, the type arguments outside generic instances withheld.
func writeListing(w io.Writer, pkgs []modulePackage) {
	for _, m := range pkgs {
		n := namer{m.module}
		fmt.Fprintf(w, "\n%s\n", m.ImportPath)
		writeFound(w, "    setup ", m.Setup, n)
		for _, t := range m.Tests {
			mark := ""
			if !t.Ran {
				mark = "  [not run]"
			}
			fmt.Fprintf(w, "  %-11s %s%s\n", t.Layer(), t.Name, mark)
			writeFound(w, "    ", t.found, n)
		}
	}
}

func writeFound(w io.Writer, indent string, f found, n namer) {
	for t := range f.Reached.set().Each {
		fmt.Fprintf(w, "%s%s: %s\n", indent, t, f.Reached[t])
	}
	var withheld []string
	for _, wh := range f.Withheld {
		withheld = append(withheld, wh.text(n))
	}
	slices.Sort(withheld)
	for _, line := range withheld {
		fmt.Fprintf(w, "%swithheld: %s\n", indent, line)
	}
}
