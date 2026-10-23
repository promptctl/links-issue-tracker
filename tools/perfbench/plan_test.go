package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The empty store is zero imports, not an empty one: `lit import` refuses a
// spec with no issues, so a batch list that yielded one empty batch would fail
// generation at the size the epic uses as its control.
func TestImportBatchesYieldsNoBatchForTheEmptyStore(t *testing.T) {
	if got := importBatches(0); len(got) != 0 {
		t.Fatalf("importBatches(0) returned %d batches, want none", len(got))
	}
}

func TestImportBatchesBuildsTheRequestedRows(t *testing.T) {
	batches := importBatches(9)
	if len(batches) != 1 {
		t.Fatalf("importBatches(9) returned %d batches, want 1", len(batches))
	}
	recs := batches[0]
	if len(recs) != 9 {
		t.Fatalf("importBatches(9) produced %d records, want 9", len(recs))
	}
	// Row sizes are derived from this repository's own export (median
	// description 1310 bytes, title 69); a row padded to some other size makes
	// the byte figures incomparable to the real store.
	for i, r := range recs {
		if len(r.Description) != descriptionBytes {
			t.Errorf("record %d description is %d bytes, want %d", i, len(r.Description), descriptionBytes)
		}
		if len(r.Title) != titleBytes {
			t.Errorf("record %d title is %d bytes, want %d", i, len(r.Title), titleBytes)
		}
		// A title drawn from the description's own stream is its first 50-odd
		// bytes verbatim, and a store compressing the two together would hold
		// the row cheaper than a real one whose title is independent prose.
		if tail := strings.TrimPrefix(r.Title, fmt.Sprintf("generated row %d ", i)); strings.HasPrefix(r.Description, tail) {
			t.Errorf("record %d title %q is a prefix of its own description", i, r.Title)
		}
		if r.LocalID == "" || r.Topic == "" || r.IssueType == "" {
			t.Errorf("record %d is missing a key `lit import` requires: %+v", i, r)
		}
	}
}

// Three groups are the smallest spec with a group before the last two, so it
// holds every edge shape the generator writes. Every task lives inside an epic
// of the same spec, and every dependency joins two tasks in different epics,
// the one form of blocked_by lit accepts. Six of the 27 tasks are blocked: every
// third task, past the first epic, which has no epic before it to wait on.
func TestImportBatchesWiresTasksInsideEpicsAndAcrossThem(t *testing.T) {
	recs := importBatches(30)[0]
	byID := map[string]storage.ImportTreeSpec{}
	for _, r := range recs {
		byID[r.LocalID] = r
	}
	epics, blocked := 0, 0
	for _, r := range recs {
		if r.IssueType == "epic" {
			epics++
			if r.Parent != "" || len(r.BlockedBy) > 0 {
				t.Errorf("epic %s has parent %q and blocked_by %v, want neither", r.LocalID, r.Parent, r.BlockedBy)
			}
			continue
		}
		if byID[r.Parent].IssueType != "epic" {
			t.Errorf("task %s has parent %q, want an epic in the same spec", r.LocalID, r.Parent)
		}
		if len(r.BlockedBy) > 0 {
			blocked++
		}
		for _, dep := range r.BlockedBy {
			target, ok := byID[dep]
			if !ok || target.IssueType != "task" || target.Parent == r.Parent {
				t.Errorf("task %s (in %s) depends on %q (%+v), want a task in another epic of the same spec",
					r.LocalID, r.Parent, dep, target)
			}
		}
	}
	if epics != 3 || blocked != 6 {
		t.Errorf("importBatches(30) made %d epics and %d blocked tasks, want 3 and 6", epics, blocked)
	}
}

// The property that matters about filler text is how well it COMPRESSES, since
// the store compresses what it holds and these rows are what the tool's byte
// figures are made of.
//
// The band is measured, not chosen: this repository's own 588 ticket
// descriptions, truncated to 1310 bytes, gzip between 1.53x and 2.56x with a
// median of 1.79x. A repeated-phrase filler gzips 20.15x, which would make
// every generated row about eleven times cheaper to store than a real one and
// understate the size figures the campaign's ceiling is derived from.
func TestFillerCompressesLikeRealTicketProse(t *testing.T) {
	const lo, hi = 1.4, 2.7
	for _, seed := range []int{0, 1, 7, 118, 589} {
		text := filler(seed, descriptionBytes)
		if len(text) != descriptionBytes {
			t.Fatalf("filler(%d) produced %d bytes, want %d", seed, len(text), descriptionBytes)
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		zw.Close()
		ratio := float64(len(text)) / float64(buf.Len())
		if ratio < lo || ratio > hi {
			t.Errorf("filler(%d) gzips %.2fx, outside the %.1f-%.1fx band real ticket "+
				"prose occupies (median 1.79x); rows this compressible do not store like "+
				"the rows being modelled", seed, ratio, lo, hi)
		}
	}
}

// Every word in the vocabulary must be reachable. A filler that misses part of
// it produces nothing that looks wrong, and the compressibility band above
// still passes: a plain LCG whose low bit strictly alternates, drawn twice per
// token against an even-length vocabulary, locks each row to half the
// vocabulary — the half its seed selects.
func TestFillerReachesTheWholeVocabulary(t *testing.T) {
	seen := map[string]bool{}
	for seed := range 200 {
		for _, w := range strings.Fields(filler(seed, descriptionBytes)) {
			seen[w] = true
		}
	}
	missing := []string{}
	for _, w := range proseWords {
		if !seen[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d vocabulary words are unreachable across 200 rows (%v); "+
			"the filler is drawing from a smaller vocabulary than it declares",
			len(missing), len(proseWords), missing)
	}
	// A single row must also see well past half, which is what a parity lock
	// caps — and this is the only assertion that can catch it, because across
	// 200 seeds the two parities between them still cover the whole vocabulary.
	//
	// Count VOCABULARY words only. Counting every whitespace token instead
	// silently defeats this: about 40% of them are random id tokens, nearly all
	// distinct, so the total sails past any threshold on those alone and the
	// check cannot fail.
	vocab := map[string]bool{}
	for _, w := range proseWords {
		vocab[w] = true
	}
	oneRow := map[string]bool{}
	for _, w := range strings.Fields(filler(0, descriptionBytes)) {
		if vocab[w] {
			oneRow[w] = true
		}
	}
	if len(oneRow) <= len(proseWords)/2 {
		t.Errorf("one row drew only %d of %d vocabulary words, at or below the half a "+
			"parity-locked generator reaches", len(oneRow), len(proseWords))
	}
}

// Two runs of the same size must generate byte-identical rows, or a change in
// reported store bytes could be the fixture moving rather than lit.
func TestFillerIsDeterministicAndVariesByRow(t *testing.T) {
	if filler(42, 200) != filler(42, 200) {
		t.Error("filler is not deterministic; store bytes would drift between runs")
	}
	if filler(42, 200) == filler(43, 200) {
		t.Error("every row got identical text; the store would deduplicate what a real one cannot")
	}
}

// Tolerance for a nonzero exit is confined to the one command that has a
// documented legitimate one, and to the one store size it is legitimate at.
// Widening any other probe's set — or accepting 6 from `next` on a populated
// store, where every generated row is open — would not fail a run or look wrong
// in the table; it would quietly let that command's failures set its fastest
// time, which is the exact defect okCodes exists to prevent, so the confinement
// is pinned rather than left to review. The expected sets are written out, not
// read from the rule under test.
func TestOnlyNextToleratesANonZeroExitAndOnlyOnTheEmptyStore(t *testing.T) {
	for _, p := range probes {
		for _, rows := range []int{0, 1, 118, 590} {
			got := fmt.Sprint(p.okCodes(rows))
			want := "[0]"
			if p.name == "next" && rows == 0 {
				want = "[6]"
			}
			if got != want {
				t.Errorf("probe %q at %d rows accepts %s, want %s", p.name, rows, got, want)
			}
		}
	}
}

// The import budget is a hang detector for a job whose length grows with the
// row count: 590 rows imported in 22s on 2026-09-27, so a fixed two minutes
// would report a few thousand rows — the natural probe past the ~1,200-row
// ceiling estimate — as a hang and kill the import mid-transaction.
func TestImportBudgetGrowsWithRowsFromTheProbeBudget(t *testing.T) {
	if got := importBudget(0); got != runBudget {
		t.Errorf("importBudget(0) = %s, want runBudget %s: the empty store has nothing to import", got, runBudget)
	}
	if got := importBudget(3000); got < 10*time.Minute {
		t.Errorf("importBudget(3000) = %s; at 37ms a row measured, 3000 rows need ~2m and a loaded machine several times that", got)
	}
	if importBudget(590) <= importBudget(118) {
		t.Error("importBudget does not grow with rows")
	}
}

// The import is the one generation step whose length grows with the row
// count, so it is the one step that must carry the row-scaled budget; every
// other step is the same job at every size and keeps the probe budget. Checked
// on the plan rather than by running it, because a budget is only observable
// by outliving it.
func TestGenerationStepsGiveTheImportTheRowScaledBudget(t *testing.T) {
	bin := litBinary{path: "/bench/lit"}
	steps := generationSteps(bin, size{name: "5x-target", rows: 590}, []importSpec{{path: "/bench/5x-target-spec-0.json", records: importBatches(590)[0]}})
	imports := 0
	for _, st := range steps {
		isImport := st.argv[0] == bin.path && st.argv[1] == "import"
		if isImport {
			imports++
			if st.budget != importBudget(590) {
				t.Errorf("import step budget = %s, want importBudget(590) = %s", st.budget, importBudget(590))
			}
			continue
		}
		if st.budget != runBudget {
			t.Errorf("step %v budget = %s, want runBudget %s", st.argv, st.budget, runBudget)
		}
	}
	if imports != 1 {
		t.Errorf("plan has %d import steps for one spec, want 1", imports)
	}
	if got := len(generationSteps(bin, size{name: "empty", rows: 0}, nil)); got != 3 {
		t.Errorf("the empty store's plan has %d steps, want 3 (git init, commit, lit init) and no import", got)
	}
}

// Generation runs its commands under the environment it is handed, not the
// harness's — the same guarantee the probes have, for the same reason: the
// store must be produced by the lit that is then measured.
func TestRunQuietRunsTheCommandUnderTheGivenEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the check is a shell test")
	}
	// Both environments are built from scratch rather than from os.Environ(),
	// so the control cannot be broken by a shell that happens to export the
	// marker.
	base := []string{"PATH=" + os.Getenv("PATH")}
	check := step{budget: 30 * time.Second, argv: []string{"sh", "-c", `[ "$PERFBENCH_MARK" = yes ]`}}
	if _, err := runQuiet(t.TempDir(), append(base, "PERFBENCH_MARK=yes"), check); err != nil {
		t.Errorf("the command did not see the environment it was given: %v", err)
	}
	if _, err := runQuiet(t.TempDir(), base, check); err == nil {
		t.Error("the command saw PERFBENCH_MARK from an environment that does not carry it")
	}
}

// A generation step that hangs must be killed and reported as a hang naming the
// command, not left to wedge the tool silently. Generation is entirely write
// path, which is where this epic's subject — a lock a writer can wait fifteen
// minutes on — actually bites.
func TestRunQuietKillsACommandOverBudget(t *testing.T) {
	_, err := runQuiet(t.TempDir(), os.Environ(), step{budget: 50 * time.Millisecond, argv: []string{"sleep", "30"}})
	if err == nil {
		t.Fatal("runQuiet returned no error for a command that outlived its budget")
	}
	for _, want := range []string{"exceeded", "budget", "sleep"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — a hang must name what hung", err, want)
		}
	}
}

// And a command that finishes inside its budget is not reported as a hang.
func TestRunQuietAcceptsACommandInsideItsBudget(t *testing.T) {
	if _, err := runQuiet(t.TempDir(), os.Environ(), step{budget: 30 * time.Second, argv: []string{"true"}}); err != nil {
		t.Errorf("runQuiet(true) error = %v, want nil", err)
	}
}

// runQuiet answers a step's stdout and nothing else: the import's id mapping
// is read from it, so stderr mixed in would put lines in front of the parser
// that are not lit's answer.
func TestRunQuietAnswersStdoutAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the check is a shell command")
	}
	out, err := runQuiet(t.TempDir(), os.Environ(), step{budget: 30 * time.Second, argv: []string{"sh", "-c", "echo answer; echo noise >&2"}})
	if err != nil {
		t.Fatalf("runQuiet error = %v", err)
	}
	if string(out) != "answer\n" {
		t.Errorf("runQuiet stdout = %q, want %q", out, "answer\n")
	}
}

// Ids come back in the spec's order whatever order lit printed them in, since
// lit creates in dependency order; a record the output does not map, and a
// local id mapped twice, are both failures rather than a shorter id list.
func TestIdsLoadedReadsTheImportMappingInSpecOrder(t *testing.T) {
	loads := []storage.ImportTreeSpec{{LocalID: "r0"}, {LocalID: "r1"}, {LocalID: "r2"}}
	out := []byte("imported 3 issues\n  r1 -> bench-b\n  r0 -> bench-a\n  r2 -> bench-c\n")
	ids, err := idsLoaded(out, loads)
	if err != nil {
		t.Fatalf("idsLoaded error = %v", err)
	}
	if got := strings.Join(ids, ","); got != "bench-a,bench-b,bench-c" {
		t.Errorf("ids = %s, want bench-a,bench-b,bench-c (spec order)", got)
	}
	if _, err := idsLoaded([]byte("imported 2 issues\n  r0 -> bench-a\n  r2 -> bench-c\n"), loads); err == nil || !strings.Contains(err.Error(), `"r1"`) {
		t.Errorf("idsLoaded with r1 unmapped: error = %v, want one naming r1", err)
	}
	if _, err := idsLoaded([]byte("  r0 -> bench-a\n  r0 -> bench-z\n  r1 -> b\n  r2 -> c\n"), loads); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("idsLoaded with r0 mapped twice: error = %v, want a duplicate refusal", err)
	}
	if ids, err := idsLoaded([]byte("Initialized empty Git repository\n  hooks: x -> y\n  hooks: x -> z\n"), nil); err != nil || len(ids) != 0 {
		t.Errorf("idsLoaded over a step that loads nothing = (%v, %v), want no ids and no error", ids, err)
	}
}

// show is the one probe that names a row, so it has an invocation against
// every store with a row and none against the empty control, where timing it
// would time the refusal path instead of the command.
func TestShowIsInvokedOnlyAgainstAStoreWithARow(t *testing.T) {
	names := func(invs []invocation) map[string][]string {
		out := map[string][]string{}
		for _, inv := range invs {
			out[inv.probe.name] = inv.args
		}
		return out
	}
	populated := names(invocationsFor(probes, generatedStore{size: size{rows: 2}, ids: []string{"bench-a", "bench-b"}}))
	if got := strings.Join(populated["show"], " "); got != "show bench-a" {
		t.Errorf("show against a populated store runs %q, want %q", got, "show bench-a")
	}
	empty := names(invocationsFor(probes, generatedStore{size: size{rows: 0}}))
	if args, ok := empty["show"]; ok {
		t.Errorf("show has an invocation against the empty store: %v", args)
	}
	if len(empty) != len(probes)-1 {
		t.Errorf("the empty store has %d invocations, want every probe but show (%d)", len(empty), len(probes)-1)
	}
}

func TestEveryProbeDeclaresItsProvingExitCodes(t *testing.T) {
	for _, p := range probes {
		for _, rows := range []int{0, 590} {
			if p.okCodes == nil || len(p.okCodes(rows)) == 0 {
				t.Errorf("probe %q declares no expected exit codes at %d rows, so any failure would be "+
					"recorded as its fastest run", p.name, rows)
			}
		}
	}
}

func TestParseSizes(t *testing.T) {
	if got, err := parseSizes(""); err != nil || len(got) != len(defaultSizes) {
		t.Errorf("parseSizes(\"\") = (%v, %v), want the default envelope", got, err)
	}
	got, err := parseSizes("0, 42,590")
	if err != nil {
		t.Fatalf("parseSizes error = %v", err)
	}
	want := []int{0, 42, 590}
	if len(got) != len(want) {
		t.Fatalf("parseSizes returned %d sizes, want %d", len(got), len(want))
	}
	for i, sz := range got {
		if sz.rows != want[i] {
			t.Errorf("size %d rows = %d, want %d", i, sz.rows, want[i])
		}
		if !strings.Contains(sz.name, "42") && i == 1 {
			t.Errorf("size %d name %q does not identify its row count", i, sz.name)
		}
	}
	for _, bad := range []string{"12x", "-1", ""} {
		if _, err := parseSizes("0," + bad); err == nil {
			t.Errorf("parseSizes(%q) accepted a value that is not a row count", "0,"+bad)
		}
	}
	// A repeated size names one workspace directory twice, and generating a
	// second store into a directory that already holds one is how an N-row
	// column comes to measure 2N rows.
	if _, err := parseSizes("118,118"); err == nil {
		t.Error("parseSizes accepted a duplicate size; both columns would name one workspace")
	}
}

// Generating into a directory that already holds a store must fail. lit init is
// idempotent and lit import appends, so the alternative is a store of 2N rows
// under an N-row label, which nothing downstream can detect — reached by
// running `--keep <dir>` twice, the comparison workflow CONTRIBUTING documents.
func TestGenerateRefusesAWorkspaceThatAlreadyExists(t *testing.T) {
	parent := t.TempDir()
	sz := size{name: "today", rows: 0}
	if err := os.MkdirAll(filepath.Join(parent, sz.name), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := generate(litBinary{path: filepath.Join(parent, "unused-lit")}, parent, sz, os.Environ(), runQuiet)
	if err == nil {
		t.Fatal("generate() into an existing directory returned no error")
	}
	if !strings.Contains(err.Error(), "fresh directory") {
		t.Errorf("error %q does not tell the caller the directory must be fresh", err)
	}
}

// Apparent size and recursive: the same store on two filesystems with different
// block sizes has to report the same number, or figures stop comparing across
// machines.
func TestStoreBytesTotalsApparentSizeRecursively(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "noms", "oldgen")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep"), make([]byte, 2_000), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := storeBytes(dir)
	if err != nil {
		t.Fatalf("storeBytes error = %v", err)
	}
	if got != 2_100 {
		t.Errorf("storeBytes = %d, want 2100 (apparent sizes, nested files included)", got)
	}
}

func TestStoreBytesReportsAMissingStore(t *testing.T) {
	if _, err := storeBytes(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("storeBytes over a nonexistent directory returned no error; a missing store is not a store of zero bytes")
	}
}
