# Event store: budgets and baselines

Status: baselines measured 2026-08-25; targets set by owner directive; scale
projections await the harness. This file is the **single home for every
number** in the event-store design — prose in charter.md/design.md cites this
file and repeats nothing. A budget change is an owner decision recorded here,
not a drift.

Two owner directives govern everything below:

1. **Every lit command completes in under 1 second** of wall time.
2. **Budgets hold at 10x current measured scale on both axes** — 10x history
   per store and 10x stores per machine — verified by a load harness before
   any migration gate passes, not after.

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

The 10x corpus is defined from these once the unmeasured rows land: the
harness replays the real fleet's largest store, then synthesizes 10x its
event count, and runs every benchmark on a machine simulating 10x the store
count.

## Command budgets (at 10x, on this class of machine)

"Compute" excludes network transfer only where physics demands it (first-clone
download); per `[INV:no-foreground-network]` no other command touches the
network in the foreground at all.

| Operation | Budget | Notes |
|---|---|---|
| Any command, warm cache | < 300 ms | the everyday case; p95 |
| Any command, cold cache (incremental fold from snapshot) | < 1 s | |
| Full refold from genesis (fold-version bump; worst case) | < 1 s | can occur inside any command; sets the codec + snapshot bar |
| First-adopt verification compute | < 1 s | the riskiest budget; forces design OPEN #3 (batch verify vs signed snapshot) |
| Staleness check (read all writer ref heads) | < 20 ms | runs at the top of every command |
| Background receive/mirror CPU per occasion | negligible against a foreground command | must not stampede at 10x store count machine-wide |

## Storage and repo-impact budgets (at 10x)

Growth is monotone by charter #7 (no truncation), so these are ceilings on
rate and footprint, not promises of shrinkage.

| Quantity | Budget | Notes |
|---|---|---|
| Event store disk per store at 10x history | ≤ 1/10 of today's Dolt store at 1x (i.e. ≤ ~19 MB where Dolt spends 190 MB) | hypothesis to verify in S1; if missed, it is a design problem to solve before S2, not a note |
| Aggregate lit disk per machine at 10x both axes | ≤ today's 1.1 GB aggregate | checked against a synthesized fleet preserving the measured size distribution (most stores are small), not 10x copies of the largest |
| Added latency to the code repo's own `git status` / `git fetch` | not measurable above noise | lit's refs and objects may not degrade the developer's ordinary git experience |
| Writer refs per repo | pruned to live checkouts + anchored sweeps | dead-ref growth is a hygiene bug, not a budget consumer |

## Harness contract

The load harness is the migration's instrument, not an afterthought:

- **Measures first.** Its first run produces the unmeasured baseline rows
  above from the real fleet before any synthetic scaling.
- **Replays reality.** The 10x corpus is synthesized from the real largest
  store's replay, preserving its shape (event-type mix, rank-intent density,
  prose sizes), not from uniform random events.
- **Gates the migration.** Design §migration's S1→S2 gate is "oracle diff
  empty **and** every table in this file green at 10x." A red row stops the
  flip; the fix happens while Dolt is still authoritative and stopping is
  free.
- **Outlives the migration.** The per-command budgets become regression
  checks in CI once S4 lands, so the <1s directive is enforced by the build,
  not by memory (the testperf epic's runtime-budget gate is the pattern).
