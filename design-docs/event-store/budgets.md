# Event store: budgets and baselines

Status: baselines measured 2026-08-25; targets set by owner directive; scale
projections await the harness. This file is the single home for every
**measured baseline and derived budget figure** in the event-store design —
prose in charter.md/design.md cites those rather than restating them. The two
governing directives — under one second per command, held at 10x on both
axes — are **charter.md hard constraint 3's**, owned there and only turned
into measurable rows here. A budget change is an owner decision recorded
here, not a drift.

## Measured baselines (2026-08-25, this machine)

| Measurement | Value | Provenance |
|---|---|---|
| lit projects under ~/code | ~30 unique (57 store dirs incl. worktree double-counts) | filesystem walk of `git-common-dir/links/dolt` |
| Aggregate Dolt store disk | ~1.1 GB | `du -sm` sum over the 57 dirs |
| Largest single store | 190 MB (links-issue-tracker) | `du -sh` |
| `lit backlog` wall time, pre-#414 binary | 10.9 s (includes inline receive fetch) | timed in links-issue-tracker |
| `lit backlog` wall time, post-#414 | ~0.2 s | PR #414's measurement; re-confirm in harness |
| Active backlog rows (this repo) | ~123 | `lit backlog \| wc -l` |
| Total tickets / total mutations per store | **unmeasured** — harness's first job | — |
| Events per week of agent churn | **unmeasured** — harness's first job | — |

The 10x corpus has **two components**, defined here once (per-row notes may
add detail but never substitute a different construction):

- **Depth corpus** — the real fleet's largest store replayed, then
  synthesized to 10x its event count preserving its shape (event-type mix,
  rank-intent density, prose sizes). Per-command benchmarks (the Command
  budgets table) run against this, as the worst case.
- **Breadth fleet** — 10x the measured store count, preserving the measured
  size *distribution* (most stores are small), not 10x copies of the
  largest. Machine-wide benchmarks (aggregate disk, background stampede) run
  against this.

## Command budgets (at 10x depth corpus, on this class of machine)

Every command row is a **total wall-time ceiling, inclusive of all
components** — the staleness row profiles a component *inside* those
ceilings, not an addition to them; there is no reading of this table under
which per-component greens can sum past a command ceiling. "Compute" excludes
network transfer only where physics demands it (first-clone download); per
`[INV:no-foreground-network]` no other command touches the network in the
foreground at all.

| Operation | Budget | Notes |
|---|---|---|
| Any command, warm cache | < 300 ms | the everyday case; p95 |
| Any command, cold cache (incremental fold from snapshot) | < 1 s | |
| Full refold from genesis (fold-version bump; worst case) | < 1 s | can occur inside any command; sets the codec + snapshot bar |
| First-adopt verification compute | < 1 s | the riskiest budget; forces design OPEN #3 (batch verify vs signed snapshot) |
| Staleness check (read all writer ref heads) | < 20 ms | component profile within the command ceilings above |
| Background receive/mirror, per occasion | ≤ 1 CPU-second | and machine-wide background CPU ≤ 5% of one core averaged over any 60 s window on the breadth fleet |

## Storage and repo-impact budgets (at 10x)

Growth is monotone by charter #7 (no truncation), so these are ceilings on
rate and footprint, not promises of shrinkage.

| Quantity | Budget | Notes |
|---|---|---|
| Event store disk per store at 10x history | ≤ 1/10 of today's Dolt store at 1x (i.e. ≤ ~19 MB where Dolt spends 190 MB) | hypothesis to verify in S1; if missed, it is a design problem to solve before S2, not a note |
| Aggregate lit disk per machine at 10x both axes | ≤ today's 1.1 GB aggregate | checked against the breadth fleet |
| Added latency to the code repo's own `git status` / `git fetch` | not measurable above noise | lit's refs and objects may not degrade the developer's ordinary git experience |
| Writer refs per repo | pruned to live checkouts + anchored sweeps | dead-ref growth is a hygiene bug, not a budget consumer |

## Harness contract

The load harness is the migration's instrument, not an afterthought:

- **Measures first.** Its first run produces the unmeasured baseline rows
  above from the real fleet before any synthetic scaling.
- **Replays reality.** Both corpus components are built from real-store
  replays per the corpus definition above — never from uniform random
  events.
- **Gates the migration.** The gate condition is **owned by design
  §migration** (S1 row) and its wording governs: the oracle diff empty over
  sustained real use, and budgets passing at 10x — where "budgets passing"
  means every row of the two budget tables above green (the baselines table
  is descriptive input, not a gate). A red row stops the flip; the fix
  happens while Dolt is still authoritative and stopping is free.
- **Outlives the migration.** The per-command budgets become regression
  checks in CI once S4 lands, so the <1s directive is enforced by the build,
  not by memory (the testperf epic's runtime-budget gate is the pattern).
