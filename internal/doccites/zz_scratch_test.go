package doccites

import (
	"os"
	"sort"
	"strings"
	"testing"
)

func TestScratchResolvedRoots(t *testing.T) {
	f, err := Survey(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]int{}
	for _, fi := range f {
		if fi.File == "" {
			continue
		}
		r := strings.SplitN(fi.File, "/", 2)[0]
		roots[r]++
	}
	var ks []string
	for k := range roots {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return roots[ks[i]] > roots[ks[j]] })
	for _, k := range ks {
		t.Logf("%6d  %s", roots[k], k)
	}
	// how many citations resolved ONLY via the bare-basename suffix fallback?
	tree, _ := Index(os.DirFS("../.."))
	n := 0
	ex := 0
	for _, fi := range f {
		if fi.File == "" || strings.Contains(fi.Named, "/") {
			continue
		}
		if _, ok := tree.lines[fi.Named]; ok {
			continue
		}
		n++
		if ex < 8 {
			t.Logf("BASENAME-FALLBACK %s:%d %s -> %s (%s)", fi.Doc, fi.DocLine, fi.Named, fi.File, fi.Verdict)
			ex++
		}
	}
	t.Logf("citations resolved purely by unique-basename-in-tree: %d", n)
}
