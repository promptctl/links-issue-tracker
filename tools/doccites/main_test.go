package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

// An empty corpus must not be reported as a clean one.
//
// Every share in the report divides by the total, so a corpus of nothing
// printed five NaN% rows and exited 0 — which reads like a measurement that
// found nothing wrong rather than an instrument that found no subject.
// [LAW:no-silent-failure]
func TestAnEmptyCorpusSaysThereIsNothingToMeasure(t *testing.T) {
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("A chapter that cites no line numbers at all.\n")},
	}
	var out bytes.Buffer
	if err := run(fsys, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to measure") {
		t.Fatalf("an empty corpus should say so: %q", out.String())
	}
	if strings.Contains(out.String(), "NaN") {
		t.Fatalf("the report divides by a total of zero: %q", out.String())
	}
}

// -list prints the citations a rule decided against, and only those. An unbound
// citation is one no rule could decide, so listing it beside the decided ones
// would bury the finding a reader came for under the rest of the corpus.
func TestListPrintsTheJudgedCitationsAndNotTheUndecidedOnes(t *testing.T) {
	doc := "`Thing` is here (`a.go:3`), and `Widget` is over there (`a.go:2`).\n" +
		"\n" +
		"Nothing names a symbol for this one (`a.go:1`).\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte(doc)},
		"a.go":              &fstest.MapFile{Data: []byte("package a\n\ntype Thing struct{}\n")},
	}
	var out bytes.Buffer
	if err := run(fsys, &out, true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "unbound") {
		t.Fatalf("the summary should still count the undecided citation: %q", got)
	}
	listed := got[strings.LastIndex(got, "\n\n"):]
	if strings.Contains(listed, "a.go:1") {
		t.Fatalf("an undecided citation was listed as a finding: %q", listed)
	}
	if !strings.Contains(listed, "`a.go:2`") {
		t.Fatalf("the citation that was judged wrong should be listed: %q", listed)
	}
}
