// Command doccites reports the specification's file:line citations against the
// code they point at.
//
// It exists so the share written into CONTRIBUTING.md can be re-derived rather
// than remembered. A measurement nobody can reproduce decays the same way the
// citations it measures do, and this corpus already has one retracted number in
// its ticket history because the instrument that produced it was never shipped
// beside it.
//
//	go run ./tools/doccites          # the corpus summary
//	go run ./tools/doccites -list    # every citation that does not hold
//	go run ./tools/doccites -sync    # rewrite the gate's manifest
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"github.com/promptctl/links-issue-tracker/internal/doccites"
)

func main() {
	list := flag.Bool("list", false, "print every citation that does not hold")
	sync := flag.Bool("sync", false, "rewrite internal/doccites/manifest_gen.go from the corpus")
	flag.Parse()

	if *sync {
		if err := regenerate(); err != nil {
			fmt.Fprintln(os.Stderr, "doccites:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.DirFS("."), os.Stdout, *list); err != nil {
		fmt.Fprintln(os.Stderr, "doccites:", err)
		os.Exit(1)
	}
}

func run(root fs.FS, out *os.File, list bool) error {
	findings, err := doccites.Survey(root)
	if err != nil {
		return err
	}
	docs, err := doccites.SpecText(root)
	if err != nil {
		return err
	}
	tally := doccites.Count(findings)

	census := map[doccites.Shape]int{}
	for _, text := range docs {
		for sh, n := range doccites.Census(text) {
			census[sh] += n
		}
	}

	if tally.Total() == 0 {
		// Every share below divides by this. Printing five NaN% rows and
		// exiting 0 reports a measured corpus of nothing, which reads like a
		// clean result rather than an instrument that found no subject.
		fmt.Fprintln(out, "no citations found in the specification — nothing to measure")
		return nil
	}

	fmt.Fprintf(out, "%d citations across the specification\n\n", tally.Total())
	fmt.Fprintln(out, "written as:")
	for sh := doccites.Qualified; sh <= doccites.Tailed; sh++ {
		fmt.Fprintf(out, "  %-13s %6d\n", sh, census[sh])
	}
	fmt.Fprintln(out)
	for v := doccites.Holds; v <= doccites.Unbound; v++ {
		fmt.Fprintf(out, "  %-13s %6d  %5.1f%%\n", v, tally[v], 100*float64(tally[v])/float64(tally.Total()))
	}
	fmt.Fprintf(out, "\n%d cite a symbol the file declares, and are therefore decidable;\n", tally.Bound())
	if tally.Bound() == 0 {
		fmt.Fprintln(out, "none of them bind a symbol, so none can be judged either way.")
	} else {
		fmt.Fprintf(out, "of those, %.1f%% hold and %.1f%% point at the wrong lines.\n",
			100*float64(tally[doccites.Holds])/float64(tally.Bound()),
			100*float64(tally[doccites.Moved])/float64(tally.Bound()))
	}

	perDoc := map[string]doccites.Tally{}
	for _, f := range findings {
		if perDoc[f.Doc] == nil {
			perDoc[f.Doc] = doccites.Tally{}
		}
		perDoc[f.Doc][f.Verdict]++
	}
	var names []string
	for d := range perDoc {
		names = append(names, d)
	}
	// Name breaks the tie: sort.Slice is not stable and names comes from map
	// iteration, so two documents with equal totals swapped places run to run
	// in a report whose whole point is being reproducible.
	sort.Slice(names, func(i, j int) bool {
		if a, b := perDoc[names[i]].Total(), perDoc[names[j]].Total(); a != b {
			return a > b
		}
		return names[i] < names[j]
	})

	fmt.Fprintf(out, "\n%6s %6s %6s %6s %6s %6s  %s\n", "total", "holds", "moved", "unres", "range", "unbound", "document")
	for _, d := range names {
		c := perDoc[d]
		fmt.Fprintf(out, "%6d %6d %6d %6d %6d %6d  %s\n",
			c.Total(), c[doccites.Holds], c[doccites.Moved], c[doccites.Unresolved], c[doccites.OutOfRange], c[doccites.Unbound], d)
	}

	if !list {
		return nil
	}
	fmt.Fprintln(out)
	for _, f := range findings {
		if f.Verdict != doccites.Holds && f.Verdict != doccites.Unbound {
			fmt.Fprintln(out, f)
		}
	}
	return nil
}

// manifestPath is where the gate reads its baseline from.
const manifestPath = "internal/doccites/manifest_gen.go"

// regenerate rewrites the manifest from the corpus as it stands.
//
// The diff it produces is the review signal, exactly as docclaims' is: an entry
// leaving the manifest is a sentence that stopped pointing at its subject, so
// regenerating without reading what left is the one use that defeats the gate.
func regenerate() error {
	src, err := doccites.Regenerate(os.DirFS("."))
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, []byte(src), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", manifestPath)
	return nil
}
