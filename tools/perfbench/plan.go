package main

import "time"

// The measurement plan: which commands are timed, at which store sizes, and
// what a generated row looks like. The one home of every number this tool
// reports against. [LAW:one-source-of-truth]
//
// WHY THESE ARE VALUES AND NOT FUNCTIONS. The scale epic's description names
// three stores ("empty, this repo's ~42 tickets, and something large enough to
// be interesting") and a handful of commands. Those are instance examples, not
// type definitions: a `benchBacklog`/`benchNext`/`benchLs` family would encode
// the variability in function names, so every new command measured would be
// new code instead of a new row here. [LAW:one-type-per-behavior]

// probe is one user-facing invocation to time — the argv after the binary, and
// the exit codes that prove the command actually did its job.
//
// okCodes is the load-bearing field, and the reason it exists is a trap this
// tool walked into during design. `lit next` exits 6 with "no ready work" on
// an empty store: that is the command answering correctly, not failing. But a
// command that fails returns FAST — a refused invocation lands in ~30ms, which
// is quicker than any real answer and would therefore win the min this tool
// reports. So an unchecked exit code turns every breakage into a record-setting
// measurement: an answer-shaped void, where "0.03s" means "did nothing".
// Declaring the proving codes is what makes a sample a fact about the command
// rather than a fact about how fast it can decline. [LAW:no-silent-failure]
type probe struct {
	// name is how the row prints, and it is written out rather than derived
	// from args so a probe can be renamed in the table without changing what
	// is invoked.
	name    string
	args    []string
	okCodes []int
	// mutates marks a probe that writes to the store. It is a field rather
	// than a separate write-probe list because the ordering it drives — every
	// read probe measured before anything writes — is then a property of the
	// data the runner sorts on, not a branch in the runner.
	// [LAW:dataflow-not-control-flow]
	mutates bool
}

// probes is the measured surface, in reading order: the two controls first,
// then the read commands, then the one write.
//
// THE TWO CONTROLS EARN THEIR PLACE. `version` and `quickstart` never open the
// store — the epic uses quickstart's 70ms precisely to prove that process
// startup is not the cost, so everything above it is store open plus query.
// Without them a whole-table regression (a slower Go runtime, a bigger binary,
// a loaded machine) reads identically to a store regression.
//
// WHAT IS DELIBERATELY ABSENT: every command that takes an issue id — `show`,
// `history`, `children`. A column is only comparable across sizes if the same
// invocation is legal at every size, and on the empty control there is no id to
// pass. Timing `show <nonexistent-id>` instead would measure the refusal path,
// which is the answer-shaped void described on okCodes wearing a different
// costume. Adding them means deciding what the empty control does first; that
// decision belongs to whoever needs the column, not to a placeholder here.
var probes = []probe{
	{name: "version", args: []string{"version"}, okCodes: []int{0}},
	{name: "quickstart", args: []string{"quickstart"}, okCodes: []int{0}},
	{name: "ls", args: []string{"ls"}, okCodes: []int{0}},
	{name: "backlog", args: []string{"backlog"}, okCodes: []int{0}},
	// 6 is "no ready work" — the honest answer on the empty store, and the
	// only size at which it can occur, since every generated row is open.
	{name: "next", args: []string{"next"}, okCodes: []int{0, 6}},
	{name: "stores --counts", args: []string{"stores", "--counts"}, okCodes: []int{0}},
	{name: "doctor", args: []string{"doctor"}, okCodes: []int{0}},
	{name: "workspace", args: []string{"workspace"}, okCodes: []int{0}},
	{name: "new (write)", args: []string{
		"new", "--title", "perfbench write probe", "--type", "task", "--topic", "bench",
	}, okCodes: []int{0}, mutates: true},
}

// size is one store to generate and measure at. rows is both the store's total
// row count and its workable count: every generated row is open, so the queue
// `backlog` and `next` walk is as long as the store itself. That is the
// conservative case and it is why these numbers are not directly this
// repository's — the live store holds 827 rows of which 118 are workable, so a
// 590-row generated store is a harder gather than the real one, not an easier
// one.
type size struct {
	name string
	rows int
}

// defaultSizes is the repository's established performance envelope, and the
// numbers come from the rule that already owns them rather than a second one
// invented here: `lit stores --counts` reports 118 workable rows today, and the
// design target is 5x the largest local backlog. internal/cli's gather
// benchmark and internal/store's list benchmarks are calibrated to the same
// 118/590 pair, so a figure from this tool and a figure from `go test -bench`
// describe the same store. [LAW:one-source-of-truth]
//
// The empty store is the control that separates fixed cost — store open, the
// migration check — from per-row cost. It is the measurement the scale epic's
// next ticket (where the time actually goes) reads first.
var defaultSizes = []size{
	{name: "empty", rows: 0},
	{name: "today", rows: 118},
	{name: "5x-target", rows: 590},
}

// The generated row's shape, derived from this repository's own export rather
// than chosen: across 827 real issues the median description is 1310 bytes and
// the median title 69. Store bytes scale with description bytes far more than
// with row count, so a row padded to an invented size would make the byte
// figures unreadable against the real store. [LAW:one-source-of-truth]
const (
	descriptionBytes = 1310
	titleBytes       = 69
)

// blockedEveryNth wires every third generated row to depend on its predecessor,
// so roughly a third of the queue is blocked. That ratio is measured, not
// picked: `lit stores --counts` reports 42 blocked against 118 workable. A
// store with no dependency edges at all would time a gather that never resolves
// a blocker — measuring a code path that does not exist in practice.
const blockedEveryNth = 3

// repeats is how many times each probe runs, and min-of-repeats is what gets
// reported.
//
// WHY MIN AND NOT MEAN. Benchmark noise on a developer machine is one-sided:
// contention, scheduling and page-cache misses only ever make a run slower,
// never faster. So the mean measures the machine's load and the min measures
// the code — and on this machine that is not a subtlety, because several agent
// sessions share the checkout and one of them compiling Go during round three
// would otherwise land in the number. The max is reported alongside so the
// contamination stays visible instead of being averaged into the result: a max
// near the min means a quiet box and a trustworthy figure, a max several times
// the min means the run was fighting for CPU and only the min survived it.
const repeats = 5

// runBudget bounds a single probe invocation. It exists so a hang is reported
// as a hang naming the probe, rather than as a tool that never returns: the
// scale epic's own subject matter is a write path that can wait fifteen minutes
// on a lock, so "slow" here is a plausible outcome and an unbounded wait would
// make this tool unable to report it. [LAW:no-silent-failure]
const runBudget = 2 * time.Minute
