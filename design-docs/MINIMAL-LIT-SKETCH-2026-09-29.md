# The simplest lit that still fits the use case

Status: snapshot, written 2026-09-29 against master `88672bf0` (v0.16.0 plus
five fixes). This is the opening move of the simplification the owner asked
for before the event-store campaign: a statement of what lit is for, the
smallest lit that still delivers it, and everything else sorted by why it can
go. Every line count below was measured with `wc -l` on non-test Go unless a
row says otherwise. Counts in shared files are ranges and overlap a little
between rows, so treat sums as approximate to a few hundred lines.

## What lit is for

lit keeps the backlog in the repository as a total order of work, and gives an
agent a loop it can run without deciding anything except how to build:
`backlog` to see the order, `next` to get the one ticket, `start` to take it,
`done` to finish it. At the moments where the agent's intent meets action, lit
injects guidance that a human wrote as data. The backlog travels through the
git remote the code already uses, so a second machine sees the same order with
no server.

That is the whole value. The README says it in four bullets, and
`design-docs/project-intent.md` narrows it further: single-agent-first, stays
minimal, and variability lives in data at the edges rather than in modes in the
code. So the test for every feature is one question: does it change what a
single agent does next, or what it learns at the moment it acts? If not, it is
a mechanism, and mechanisms are judged by whether the store swap needs them.

Measured against that test, the tree today is about one part product to two
parts mechanism.

| Bucket | Non-test Go lines | Share of the 70,623 shipped |
|---|---|---|
| Serves the loop, kept | ~22,000 | ~31% |
| Cut now, independent of the store | ~18,000 | ~25% |
| Exists only because the store is Dolt | ~30,000 | ~43% |

The middle row is the point of this document. It can go before the campaign,
it shrinks the event schema the new store has to carry from day one, and it
takes about the same number of test lines with it.

## The sketch

### Commands

Twenty-five commands instead of forty-two. Sub-verbs are listed where they
survive.

| Group | Commands | Change from today |
|---|---|---|
| The loop | `backlog`, `next`, `start`, `done`, `close [--reason]`, `open` | `next` becomes "first ready row"; `close` loses `--resolution` and `--of`; `start` loses `--take` |
| Recording work | `new`, `import` (YAML), `update`, `comment add`, `label add`, `label rm`, `show`, `history`, `ls` | `followup` folds into `new --parent`; `ls` keeps `--status --parent --labels --search --limit`; `comment rm` and `show --field` go |
| Structure | `parent set`, `parent clear`, `dep add`, `dep rm`, `rank --top/--bottom/--above/--below` | `children` folds into `ls --parent`; `dep --type parent-child` and `rank set` go |
| Sync | `sync` | push and pull collapse into one verb; the fetch, status, remote, reconcile, compact and take verbs go |
| Maintenance | `init`, `doctor`, `version`, `export`, `stores --counts` | `workspace` and `prefix` fold into `doctor` and `init`; `upgrade` is `scripts/install.sh` |
| Guidance | `quickstart [topic]`, `workflows` (overview, `show`) | `workflows edit`, `quickstart --eject/--refresh`, `completion`, `hooks` go |

Gone outright: `archive`, `unarchive`, `delete`, `restore`, `orphaned`, `bulk`,
`backup`, `snapshots`, `lifeboat`, `downgrade`, `upgrade`, `hooks`, `prefix`,
`workspace`, `completion`, `followup`, `children`.

### The issue

One record, one status axis, two shapes.

| Part | Minimal form | Today |
|---|---|---|
| Fields | id, title, description, assignee, rank, labels, status, created/updated/closed timestamps, topic (see decisions) | plus `prompt`, `priority`, `lane`, `resolution`, `redirect_target`, `archived_at`, `deleted_at` |
| Shape | container or leaf; the type vocabulary is schema data, as the workspace-schema design already requires | five hard-coded types, of which only `epic` changes behavior |
| Status | `open`, `in_progress`, `closed`; a container's status is folded from its children | the same, plus a second retention axis (`live`, `archived`, `deleted`) |
| Relations | `parent-child`, `blocks`, `related-to` | the same |
| Also | comments, labels, an append-only event log | the same, with per-event stream and workspace attribution used only by claims |

Labels stay because they are the one place policy already lives as data: the
`needs-design` and `focus` labels, and every workflow definition, key on them.

### Readiness and order

A leaf is workable when it is open, every `blocks` edge into it or into an
ancestor epic is closed, and no earlier sibling under the same epic is
unfinished. Rank is the only sequencer: composite rank (epic rank, then own
rank), with no priority tier, no focus scope, no lane. `next` is the first
workable row, or the row already in progress. A `blocks` edge whose source
sits inside the epic it blocks is refused at write time, which removes the
wait-loop fixpoint that exists to tolerate it.

### Configuration

No keys. Of the twelve keys in `config.go` today, five are never read
(`logging.verbose`, `logging.file`, `init.install_hooks`,
`init.install_agents`, `migration.auto_apply`; ticket `links-config-cxki`),
three belong to features cut here (`quickstart.soil_mode`,
`snapshot.retention_budget`, `claims.freshness_window`), three configure sync
modes the event store removes (`sync.cadence`, `sync.receive`,
`sync.owner_notify_cmd`), and `ready.required_fields` is already slated to
become a transition guard in the workspace schema. The one switch that
survives is the `LIT_DISABLE_AUTO_SYNC` environment variable for CI. When the
workspace schema file lands, it is the configuration.

### Guarantees the rewrite must keep

These are the user-visible promises hiding inside the mechanism. The sync and
data-safety slices are 31,700 lines that deliver these eleven sentences.

1. A write reaches the remote on its own, and no command waits on the network.
2. Other machines' writes arrive on their own, off the foreground path.
3. The first command on a fresh clone sees the real backlog.
4. One manual "sync now" verb exists.
5. The agent is told early when its view is stale or its pushes are failing.
6. A concurrent rewrite of free text is shown to the agent to merge, never
   picked silently; a duplicate id gets an advisory.
7. An older binary refuses data it cannot read and names the fix.
8. All data comes out as one versioned JSON export.
9. `doctor` names the store, its freshness, and any parent or dependency cycle.
10. Ids carry a readable project prefix that every machine agrees on (today the
    prefix is per-clone and not synced).
11. The agent learns lit exists from `AGENTS.md`/`CLAUDE.md` and finds the
    guidance router at `lit quickstart`.

## What goes, and why

### Cut now: it does not serve a single agent's next step

About 18,000 non-test lines, roughly the same in tests, and the spec tree.
None of it depends on the store, so all of it can land before the campaign
and shrink what the campaign has to port.

**Multi-checkout coordination (about 4,300 lines).** Claims are derived at
read time from event attribution, a six-hour freshness window and worktree
liveness; `next` routes around held lanes in four steps; `start` gates
takeovers. For one checkout none of it fires: the routing steps that need a
held lane never run, and nothing ever contests. Both storage engines import
the claims package just to decide whether a repeated `start` is a no-op, and
every `backlog`, `next` and `start` pays a full scan of issues, relations,
all events and `git worktree list` to compute it. Lanes exist only as the unit
a claim holds. Cutting the set removes two event columns and a migration, the
per-worktree stream file, `--take`, and the seven-outcome routing taxonomy.
`next` becomes about twenty lines.

| Piece | Lines |
|---|---|
| `internal/claims`, `cli/claims_*`, stream and checkout discovery, config | ~2,650 |
| `next_route.go` and the advice code in `next.go` | ~700 |
| lanes (`LaneID`, the column, the flag on `new`/`update`) | ~100 |
| tests that go with them | ~6,000 |

**Second authorities over "what is next" (about 750 lines).** Rank is the only
sequencer by design, yet three other things reorder or scope the queue. The
`focus` label narrows backlog and the global pool to one goal's prerequisite
closure (about 280 lines) when `rank --top` on that goal does the same job.
Priority is a two-value tier sorted after composite rank, so it beats rank
(9 lines of sort, one column, three flags). The rank-inversion warning and
`doctor --fix rank` re-sort the backlog by topological order (about 330 lines)
to fix a display artifact: readiness already blocks the dependent regardless
of its rank. `orphaned` (108 lines) reports a signal its own doc says is
unreliable, since comments do not bump `updated_at`.

**A second state axis and enum fields nothing reads (about 900 lines).**
Archive and delete are a retention axis beside status, with a twelve-cell
action table, twenty-one `deleted_at` predicates in the rank code and two
`ls` flags; closed is already out of the flow, and the charter forbids
physical deletion anyway. `resolution` and `redirect_target` are written by
`close` and read by nothing that decides anything: the comment in
`resolution.go` says the next selector reads them, and no selector does.
Removing them removes migrations 00003 and 00004. `related-to` edges are
display only, but the trampoline register calls them a hard requirement, so
they stay. The `prompt` field is display only and overlaps `description`.

**Duplicate surfaces for one action (about 1,300 lines).** `ls --query` is a
second grammar whose fourteen terms duplicate thirteen flags (336 lines plus
248 test). `bulk` is a shell loop that also skips the workflow events `close`
and `label` fire, so it mutates without guidance (191 lines). The JSON tree
import is a strict subset of the YAML import (about 360 lines). `followup` is
`new --parent` with a drifted flag set (74 lines). `children` is `ls --parent`.
`dep add --type parent-child` duplicates `parent set`. `rank set` is N
`--below` calls (272 lines). `comment rm` is the only hard delete in normal
operation. `show --field` and eight `ls` flags (`--at`, `--sort`, `--format
table`, `--updated-*`, `--has-comments`, `--ids`, the `parent` and `blocked`
columns) serve no step of the loop.

**Guidance plumbing that is not the substrate (about 2,000 lines).** The
substrate is four things: the `workflows.Dispatch` seam with its event
catalog, the `quickstart` router, the managed section in `AGENTS.md`, and the
`<agent-instructions>` envelope. Around it sit two parallel layered-markdown
loaders (`internal/templates` and `internal/workflows/load.go`) with different
override directories and different customization commands, `workflows edit`
(271 lines of scaffolding), `quickstart --eject` and `--refresh` (init is
already idempotent), `completion` (238 lines for humans at a shell), the
pre-push hook (load-bearing only under the `on-push` cadence), three trace
writers that nothing reads or prunes (292 lines), and the Claude plugin, which
hard-codes one user's `gh` and `memento` workflow and cannot be overridden the
way a workflow definition can. Thirteen hard-coded "deeper guidance"
breadcrumbs should become embedded workflow definitions, which needs the
catalog extended to `label`, `parent`, `dep` and `rank`, since those dispatch
nothing today.

**Maintenance verbs the log makes redundant (about 1,000 lines).** `backup`
keeps rotating JSON copies of a store whose every prior state the log already
holds, and its "unsynced since last sync base" guard is a leftover of the
retired JSON file sync: the only writer of that marker is a previous restore.
`downgrade` reverses SQL migrations and forces every migration to ship a Down
section under two CI gates. `internal/doltcli` has no importer at all.

**Sync verbs that are plumbing exposed as commands (about 1,300 lines).**
`sync fetch`, `sync status`, `sync remote ls`, `sync reconcile abort` (a
no-op by design), the `cadence` and `receive` keys, the owner-notify hook
(four of its five kinds cannot occur after the rewrite), the sync decision
trace, and the pre-push hook.

**Developer-only weight (about 6,200 Go lines and the 18,316-line spec).**
`doc-v1-total` is a code-only specification of v1 down to SQL and message
text, held in sync by `docclaims` (a 1,134-entry manifest that every changed
user-facing message regenerates), `docnames`, and `docclaims-sync`. It
describes the surface this document cuts and the store the campaign replaces,
and it accounted for about 60 of the last 245 commits. The in-memory storage
engine (2,892 lines) exists to prove the storage seam against a second
implementation and has no non-test importer; the conformance suite it runs
(2,403 lines, 59 cases) is worth keeping as the acceptance test for the event
store. `lawtokens` stays: the token gate is doctrine, not weight.

### Dies with Dolt: do not port it

About 30,000 lines the charter already schedules for deletion at S4. They are
listed so nobody carries them across by habit.

| Mechanism | Lines | Replaced by |
|---|---|---|
| Schema migrations, reconcile-to-baseline, quarantine, snapshots before write | 4,056 | upcasters over immutable events |
| Field-aware three-way merge and reconcile, combine, prose hold | ~2,700 | the fold; keep the per-field win table in `merge/resolve.go` as the spec of the fold rules |
| Lifeboat dump and recover, shape maps | 2,790 | nothing; a content-addressed log does not need a rebuild path |
| Background mirror: beacon, single-flight lock, clone, push from clone | ~1,950 | fire-and-forget `git push refs/lit/*` |
| Locks: workspace, commit, holder | 1,659 | nothing (charter constraint 2) |
| Vendored driver and two forks | 1,670 plus 7 patches | nothing |
| Background receive: ask, clone, land | ~1,560 | `git fetch` and a refold |
| Foreground push with compaction and cache prune | ~1,250 | idempotent push |
| Upgrade installer and manifest resolver | 1,136 | `scripts/install.sh` plus a min-reader stamp on the log |
| Filesystem snapshots (four kinds, four retention budgets) | 1,223 | nothing |
| Compaction, backstop, `--full` | ~820 | `git gc` |
| Init-time adopt of the remote backlog | 757 | a fetch |
| Take approval and unrelated-history detection | ~700 | nothing; a union of event sets has no unrelated root |
| Sync-failure block and exit-5 contract | 533 | a small advisory renderer |
| Doctor checks 1-6 (constraint verify, orphan rows, ordering conventions) and the SQL restore path | ~500 | nothing; the fold cannot produce them |
| Rank smoothing and the no-room retry | ~200 | a simpler key scheme chosen with the event schema |
| Cross-toolchain build image, cgo scripts, license bundle sized by Dolt's 132 modules | 463 + 67 + 3,406 | a plain `go build`; a license tool sized for ten modules |

The binary today links 150 modules and 1,000 non-standard packages; 132 of
those modules and 929 of those packages come in only through the Dolt driver.
After the swap the dependency list is cobra, viper, `x/mod`, `primitives`,
`uuid` and `yaml`.

### Keep the guarantee, rewrite the mechanism

The eleven guarantees above. Each is a few dozen lines over append-only refs
where it is a thousand over mutable rows. The two worth naming because they
are easy to lose: staleness banners on read and write commands (about 540
lines today) are the "earlier, cheaper feedback" the intent document asks for,
and they survive as a comparison of local refs against remote-tracking refs
plus one push-outcome marker; and the schema-ahead guard (187 lines) survives
as a refusal of any event version the binary does not know.

## Decisions only the owner can make

Each has my recommendation first. Everything not listed here I have decided
above.

1. **Claims.** The workspace-schema design (principle 3) says multi-agent
   support layers on later as "multiple claimants against the same order (the
   claims machinery that already exists)". Cutting claims now contradicts that
   sentence literally but not in substance: the event store stamps a
   principal on every event by birth requirement 6, so claims re-derive there
   as a fold over attribution, cheaper than porting 2,650 lines built for
   Dolt rows. Recommendation: cut now, and amend that sentence to "the claims
   fold, rebuilt over event attribution".
2. **Lanes.** Without lanes an epic's children run strictly in rank order, so
   a catch-all epic of independent bugs serializes, and one blocked bug holds
   everything ranked after it. Recommendation: cut; independent work belongs
   in separate epics or at the top level, and a schema-declared `unordered`
   container is the named future word if it is ever needed.
3. **Priority.** Recommendation: cut the field, not just the sort. `rank
   --top` is what "urgent" means for one agent.
4. **Topic in the id.** It is required on `new`, immutable, and used only for
   the id namespace and search. Cutting it changes the id shape, which is
   churn across every ticket and doc while the store is about to move.
   Recommendation: keep it through the campaign; revisit when ids are minted
   by the event store.
5. **The v1 spec and its gates.** Every cut above must edit the spec and
   regenerate the manifest, or the build fails; that tax roughly doubles the
   cost of each cut ticket, for a document describing a surface that will not
   exist on the new store. Recommendation: retire `doc-v1-total`, `docclaims`,
   `docnames` and `docclaims-sync` in the first PR of the cut campaign, with
   the v0.16.0 tag as the permanent record of what v1 was. Keep `docsclaims`;
   it was built for the swap.
6. **The `done` gate.** `lit done` closes an open ticket today while its help
   line says it requires in_progress (ticket `links-cli-fr6r`).
   Recommendation: drop the claim rather than add the wall; the grooves
   doctrine reserves refusals for incoherent state, and finishing unclaimed
   work is not that.

## Sequence

The gate ticket `links-gate-yz7t` already says the campaign starts when every
known bug is closed and the CLI surface is straight. The cut list above is
what "straight" means, so it belongs in front of the event-store epic as its
own epic, one ticket per row of the "cut now" section, ranked so the cuts
that shrink the event schema land first: retention axis, resolution and
redirect, claims columns and lanes, priority, prompt. The store-bound rows
stay where the charter puts them, at S4, and this document is the list that
stops any of them being ported in the meantime.

Two things landed today: the dead-config-keys ticket and the `done` gate
ticket, both wired as blockers of the gate. Nothing else has been changed.

## Method and what was not verified

Six read-only tracing passes covered the core model, scheduling and claims,
sync, data safety and schema, guidance and onboarding, and developer tooling;
each measured its own line counts. I re-checked by hand the claims the
recommendations lean on: the five unread config keys, the missing importers
for the memory engine and the dolt CLI wrapper, the two storage engines
importing the claims package, `ListTopics` and `ReplaceLabels` having only
conformance callers, and `lit done` closing an open ticket in a scratch
repository built from this commit.

Not verified, and worth a look before the corresponding ticket is written:
whether the composite display order and the raw global rank order can
disagree for nested epics (the inversion check compares raw keys); and
whether `AnnotatedIssue`'s JSON form has any output path at all.
