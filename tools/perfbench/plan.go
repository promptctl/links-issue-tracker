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

// probe is one user-facing command to time — the argv it runs against a store,
// and the exit codes that prove the command actually did its job.
//
// okCodes is the load-bearing field. `lit next` exits 6 with "no ready work" on
// an empty store: that is the command answering correctly, not failing. But a
// command that fails returns FAST — a refused invocation lands in ~30ms, which
// is quicker than any real answer and would therefore win the min this tool
// reports. So an unchecked exit code turns every breakage into a record-setting
// measurement: an answer-shaped void, where "0.03s" means "did nothing".
// Declaring the proving codes is what makes a sample a fact about the command
// rather than a fact about how fast it can decline. [LAW:no-silent-failure]
type probe struct {
	// name is how the row prints, and it is written out rather than derived
	// from argv so a probe can be renamed in the table without changing what
	// is invoked.
	name    string
	argv    argvRule
	okCodes exitRule
	// mutates marks a probe that writes to the store. It is a field rather
	// than a separate write-probe list because the ordering it drives — every
	// read probe measured before anything writes — is then a property of the
	// data the runner sorts on, not a branch in the runner.
	// [LAW:dataflow-not-control-flow]
	mutates bool
}

// argvRule is the invocation a probe makes against a store whose rows carry
// these ids, in import order: its one argv, or false when the store holds
// nothing the command can be pointed at.
//
// "This store has no legal invocation of this probe" is a case with its own
// meaning: the empty control has no row for `show` to name, and timing
// `show <nonexistent-id>` instead would time the refusal path, the
// answer-shaped void okCodes exists to refuse. A probe with no invocation is
// simply not measured at that size, and the report prints its cell as absent.
// It is at most one argv because a sample, a report cell and a breach are each
// keyed by probe name: a second argv would be a second figure under one name.
// [LAW:types-are-the-program]
type argvRule func(ids []string) (args []string, ok bool)

// fixed is the rule for every command that takes no id: the same argv against
// every store.
func fixed(args ...string) argvRule {
	return func([]string) ([]string, bool) { return args, true }
}

// exitRule names the exit codes that prove a probe did its work against a
// store of a given row count.
//
// It is a function of the store's size and not a fixed set because the proving
// code genuinely moves with the size for one command: on the empty store `lit
// next` can only answer "no ready work" (6), and on a populated one, where
// every generated row is open, only a pick (0) proves it ran. A fixed {0, 6}
// would accept 6 at 590 rows — a routing regression that passes over every row
// exits in ~30ms and would set that command's fastest time — which is the
// answer-shaped void okCodes exists to refuse. [LAW:dataflow-not-control-flow]
// the accepted set is a value derived from the store, not a branch in the
// runner.
type exitRule func(rows int) []int

// exits is the rule for every command whose proving codes do not depend on the
// store's size.
func exits(codes ...int) exitRule {
	return func(int) []int { return codes }
}

// invocation is a probe resolved against one store: the argv it runs there and
// the exit codes that prove it there.
type invocation struct {
	probe   probe
	args    []string
	okCodes []int
}

// invocationsFor resolves probes against store, in the order given, dropping
// any probe the store admits no invocation of. It is the one place a probe
// meets a store, so both ways a store shapes a probe, the argv it can name and
// the codes that prove it at this size, are read here and nowhere else.
// [LAW:single-enforcer]
func invocationsFor(ps []probe, store generatedStore) []invocation {
	var out []invocation
	for _, p := range ps {
		if args, ok := p.argv(store.ids); ok {
			out = append(out, invocation{probe: p, args: args, okCodes: p.okCodes(store.size.rows)})
		}
	}
	return out
}

// probes is the measured surface, in reading order: the two controls first,
// then the read commands, then the one write.
//
// THE TWO CONTROLS EARN THEIR PLACE. `version` and `quickstart` never open the
// store — the epic uses quickstart's 70ms precisely to prove that process
// startup is not the cost, so everything above it is store open plus query.
// Without them a whole-table regression (a slower Go runtime, a bigger binary,
// a loaded machine) reads identically to a store regression.
var probes = []probe{
	{name: "version", argv: fixed("version"), okCodes: exits(0)},
	{name: "quickstart", argv: fixed("quickstart"), okCodes: exits(0)},
	{name: "ls", argv: fixed("ls"), okCodes: exits(0)},
	{name: "backlog", argv: fixed("backlog"), okCodes: exits(0)},
	// 6 is "no ready work" — the honest answer on the empty store, and the
	// only size at which it can occur, since every generated row is open. The
	// one branch here is the store's own discriminator, empty or populated.
	{name: "next", argv: fixed("next"), okCodes: func(rows int) []int {
		if rows == 0 {
			return []int{6}
		}
		return []int{0}
	}},
	// The first imported row, which is always an epic. Its children make
	// `show` print the plan of a real epic, the relations work an agent's
	// `lit show` pays.
	{name: "show", argv: func(ids []string) ([]string, bool) {
		if len(ids) == 0 {
			return nil, false
		}
		return []string{"show", ids[0]}, true
	}, okCodes: exits(0)},
	{name: "stores --counts", argv: fixed("stores", "--counts"), okCodes: exits(0)},
	{name: "doctor", argv: fixed("doctor"), okCodes: exits(0)},
	{name: "workspace", argv: fixed("workspace"), okCodes: exits(0)},
	// An epic, because it is the one ticket lit files with no parent: every
	// other type names the epic it lives inside, and the empty control has
	// none to name, so a task here would be a different invocation per size.
	{name: "new (write)", argv: fixed(
		"new", "--title", "perfbench write probe", "--type", "epic", "--topic", "bench",
	), okCodes: exits(0), mutates: true},
}

// latencyBudget is the most any probe may take, at any store size: the scale
// epic's destination, "every lit command returns in under a second", and not
// a figure fitted to what the commands cost today. A probe is judged on its
// min across rounds, because noise on a shared machine only ever adds time.
//
// It is one value for every probe because every probe has the same budget,
// and it is a ceiling, not a ratchet: no baseline is recorded and no run is
// compared with an earlier one. A ratchet fails on runner noise, gets muted,
// and then guards nothing. [LAW:one-source-of-truth]
const latencyBudget = time.Second

// probeKillAfter is when a probe invocation is killed rather than waited on.
// It is derived from the budget because a kill is only ever a verdict on it:
// any run killed here is 30 times over. The factor is 30 so the slowest figure
// the scale epic has recorded, `lit backlog` at 11.3s, is still a measurement
// rather than a kill.
const probeKillAfter = 30 * latencyBudget

// limits are the two figures a probe is held to. They travel as a value so a
// test can drive a breach and a kill in milliseconds instead of seconds.
type limits struct {
	budget time.Duration
	kill   time.Duration
}

// gateLimits are the limits every real run uses.
var gateLimits = limits{budget: latencyBudget, kill: probeKillAfter}

// size is one store to generate and measure at. rows is the store's TOTAL row
// count, epics included, and it is not the same as its workable count: one row
// in rowsPerEpic is an epic, an epic's children wait on each other in rank
// order, and blockedEveryNth wires a third of the tasks behind another epic's
// work. `lit stores --counts` on a generated 590-row store reports 40 ready
// and 491 blocked; on a 118-row store, 8 and 98. The live store's 118 open
// rows are 47 ready, because its epics run children in parallel lanes, and
// `lit import` sets no lane, so a generated epic is one lane, ready only at its
// head.
//
// Both numbers matter and neither is this repository's directly. A generated
// store is not a scale model of this repository and is not meant to be — it
// is a controlled point on the row axis, which is the axis the ceiling and the
// latency budget are both argued in. `lit backlog` reads every open row either
// way, ready or not.
type size struct {
	name string
	rows int
}

// defaultSizes is the repository's established performance envelope, and the
// numbers come from the rule that already owns them rather than a second one
// invented here: `lit stores --counts` reports 118 workable rows today, and the
// design target is 5x the largest local backlog. internal/cli's gather
// benchmark and internal/store's list benchmarks are calibrated to the same
// 118/590 pair, so this tool and `go test -bench` measure at the same points on
// the row axis, over stores of different shape. [LAW:one-source-of-truth]
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

// rowsPerEpic is how many generated rows make one epic and its children: the
// epic, then nine tasks. The ratio is this repository's own: 98 epics among 985
// issues, a mean of 9.1 children each (median 4, since one catch-all epic holds
// 391). The mean is the figure kept because it fixes how many of a store's rows
// are epics, and every command that walks the whole table reads that mix.
const rowsPerEpic = 10

// blockedEveryNth wires every third generated task to depend on a task in the
// previous epic, so roughly a third of the tasks wait on another epic's work.
// Rank order already holds every task behind its epic's head, so the edges
// change readiness only at a head; they are here so the gather resolves
// blocked_by edges, which a store gated by rank alone would never time — a
// code path every real store exercises.
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

// runBudget bounds a single generation, compaction or history step. It exists
// so a hang is reported as a hang naming the step, rather than as a tool that
// never returns: the scale epic's own subject matter is a write path that can
// wait fifteen minutes on a lock, so "slow" here is a plausible outcome and an
// unbounded wait would make this tool unable to report it.
// [LAW:no-silent-failure] Probes are not bounded by it; probeKillAfter does
// that, because a probe's limit is a verdict on the latency budget and a
// step's is not.
const runBudget = 2 * time.Minute

// importBudget bounds the one `lit import` that populates a store. It grows
// with the row count because the work does: measured on 2026-09-27
// (darwin/arm64, 12 CPUs), importing 590 rows in one batch took 22s, about
// 37ms a row, so a fixed runBudget would report `--sizes 3000` — the natural
// next probe once the ceiling estimate says ~1,200 rows — as a hang and kill
// the import mid-transaction. The allowance is several times the measured
// rate so a loaded machine still finishes inside it, on top of runBudget so
// the empty store's zero rows keep the same hang detection every probe has.
func importBudget(rows int) time.Duration {
	return runBudget + time.Duration(rows)*importRowAllowance
}

const importRowAllowance = 250 * time.Millisecond
