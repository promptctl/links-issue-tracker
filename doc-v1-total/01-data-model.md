# Data model

lit's unit of work is the **issue**. Epics are not a separate record type: an epic is an issue whose type is `epic`, and its open/closed state is computed from its children at read time, never stored. Around the issue sit eight persisted record kinds — relations, comments, labels, events, event field-changes, plan reviews, purge records, and workspace metadata — plus several derived (never-stored) concepts: epic state, lanes, claims, and readiness annotations.

This document covers the records and the value vocabularies they use. Where a rule has edge cases, the exact behavior is stated; `file:line` citations point at the defining source.

## The Issue record

An issue carries these persisted fields (`internal/model/model.go`):

| Field | Type | Notes |
|---|---|---|
| `id` | string | `<prefix>-<topic>-<hash>`; see [Identifiers](#identifiers) |
| `title` | string | |
| `description` | string | |
| `prompt` | string | optional free-text working instructions; omitted from JSON when empty |
| `plan` | string | optional free text: how the work will be done, written before it starts so a reviewer can judge it against the laws; each recorded text is a version a [plan review](#export-format) names; omitted from JSON when empty; stored in the column `work_plan` (`model.go`) |
| `priority` | int | `0` = normal, `1` = urgent; the only two values |
| `issue_type` | string | `task`, `feature`, `bug`, `chore`, or `epic` |
| `topic` | string | slug embedded in the ID; see [Identifiers](#identifiers) |
| `assignee` | string | owner identity; orthogonal to status and preserved across every transition (`model.go`). Trimmed, empty meaning unassigned; a comma is refused by every command that sets one, because every assignee filter reads a comma as the separator between names (`internal/model/assignee.go`) |
| `rank` | string | fractional-index ordering key; `""` means unranked; see [Ranking](#ranking) |
| `lane` | string | partitions an epic's children into parallel sequences; see [Lanes](#lanes) |
| `labels` | []string | normalized lowercase; see [Labels](#labels) |
| `follows_up` | string | id of the ticket this one was filed as a follow-up of (`lit followup --on`); a birth fact, set at creation and not carried by `UpdateIssueInput`; omitted from the wire when empty (`model.go`) |
| `created_at`, `updated_at` | timestamp | |
| status fields | | `status`, `closed_at`, `resolution`, `redirect_target` — present on the wire only for non-epics; see [Status lifecycle](#the-status-lifecycle) |
| retention fields | | `archived_at`, `deleted_at` — the wire encoding of the retention axis; see [Retention](#the-retention-axis) |

There is no stored `progress` field; an epic's progress counts are computed (`model.go`).

### Issue types

Five types (`internal/model/issue_type.go`): `task`, `feature`, `bug`, `chore`, `epic`. Exactly one, `epic`, is a **container** (`issue_type.go`). Container-ness is decided by the type alone, never by whether the issue has children (`model.go`). Type parsing lowercases and trims; anything outside the five values is rejected (`issue_type.go`).

Containers differ from leaves in three ways:

1. **Derived state.** An epic's state is computed from its children: all children closed (and at least one child) → `closed`; any child in progress or closed → `in_progress`; otherwise → `open`. An epic with no children is `open` (`internal/model/lifecycle/all_of.go`). Progress is the field-wise sum of every non-container descendant's counts (`all_of.go`).
2. **No direct transitions.** Applying any status action to an epic returns a typed `ContainerActionError` whose message varies with child progress — no children, all done, or N unfinished (`model.go`).
3. **No status on the wire.** A serialized epic carries no `status`, `closed_at`, `resolution`, or `redirect_target` keys (`model.go`), and JSON alone cannot reconstruct an epic's lifecycle — a decoded epic is marked pending until the store re-derives its state from children (`model.go`).

### Priorities

Two values (`internal/model/priority.go`): `0` (normal) and `1` (urgent). Both spellings — the number and the display word — come from one table, `priorityVocabulary` (`priority.go`), which every direction reads: `priorityEntry` resolves an int onto it, `String` renders through it, `ParsePriorityName` inverts it, and `Priorities` lists it. `CanonicalPriority` maps every other integer — negative or ≥2 — to normal (`priority.go`). Two strict gates share the domain and the refusal `errInvalidPriority`: `ParsePriority(int)` for the import/bulk payloads and `ParsePriorityName(string)` for the `--priority` flag, the latter accepting the word or the decimal and lowercasing/trimming first.

## The status lifecycle

A leaf issue's status is one of three states (`internal/model/lifecycle/lifecycle.go`):

- `open`
- `in_progress`
- `closed`

State parsing lowercases, trims, and accepts `in-progress` as an alias for `in_progress` (`lifecycle.go`). Lenient boundaries (import, hydration, storage) default unparseable states to `open`; strict boundaries (CLI flags, query language) reject them (`lifecycle.go`).

### Transition actions

Eight named actions, split across two independent axes (`lifecycle.go`):

| Action | Axis | Effect |
|---|---|---|
| `start` | status | → `in_progress`; the only action that carries and rewrites the assignee (`action.go`) |
| `done` | status | → `closed` with **no** resolution (the neutral success close) |
| `close` | status | → `closed` with a mandatory outcome (resolution) |
| `reopen` | status | → `open`; clears `closed_at`, resolution, and redirect target (`status_states.go`) |
| `archive` | retention | live → archived |
| `unarchive` | retention | archived → live |
| `delete` | retention | live or archived → deleted |
| `restore` | retention | deleted → live |

Status transitions are **target-state**, not edge-guarded: an action names the destination state, and applying it from any state succeeds. There is no enforced precondition (e.g. `done` does not require `in_progress` — see the discrepancy note in `06-issue-commands.md`). Applying an action whose target equals the current state returns the issue unchanged — in particular, re-closing a closed issue preserves its existing resolution and `closed_at` rather than rewriting them (`status_states.go`). Transitioning into `closed` stamps `closed_at = now (UTC)` (`status_states.go`).

The type system separates the two axes: retention actions are not status actions, so applying `archive` to the status machine is unrepresentable rather than checked (`action.go`).

### Resolutions and redirects

`close` requires an **outcome**; `done` records none. Four resolutions (`internal/model/lifecycle/resolution.go`):

| Resolution | Kind | Payload |
|---|---|---|
| `duplicate` | redirecting | `of <canonical-ticket>` — the redirect target |
| `superseded` | redirecting | `by <replacing-ticket>` — the redirect target |
| `obsolete` | terminal | none |
| `wontfix` | terminal | none |

A **redirect target** — the ticket the work went to instead — can exist only on a closed issue whose resolution is `duplicate` or `superseded`; the constructor drops a target supplied beside a terminal or absent resolution (`status_states.go`). Targets are normalized: trimmed, with blank collapsing to absent (`status_states.go`). At the type level a closed issue *may* lack `closed_at`, resolution, or (for redirecting closes) the target — legacy rows and field-wise merges can produce those — but every new `close` via the CLI requires an outcome, and new redirecting closes require a target (`status_states.go`).

Resolution parsing trims but does **not** lowercase (`resolution.go`), unlike state and type parsing.

## The retention axis

Retention is a second lifecycle axis, orthogonal to status: `live`, `archived`, or `deleted` (`internal/model/lifecycle/retention.go`). Archived-and-deleted simultaneously is unrepresentable. On the wire and in storage the axis is encoded as two nullable timestamps, `archived_at` and `deleted_at`; decoding gives deletion precedence, so a legacy row with both set reads as deleted (`retention.go`).

- **Archived**: hidden from default listings but still occupies rank space; reversible via `unarchive`.
- **Deleted**: hidden from default listings and excluded from rank space; reversible via `restore`.

The complete transition table (`retention.go`):

| current \ action | archive | unarchive | delete | restore |
|---|---|---|---|---|
| live | → archived | error | → deleted | error |
| archived | error | → live | → deleted | error |
| deleted | error | error | error | → live |

Deleting an archived issue drops the archive stamp, so a later `restore` always lands on `live`, never back on `archived` (`retention.go`). Permanent removal is not a retention state: a purge deletes the issue's row and leaves a purge record (see [Export format](#export-format)).

An issue that is archived or deleted is **frozen**. The single program-wide definition of "still unfinished work" is `InPlay`: not frozen and not closed (`model.go`). Readiness gating and claim derivation both read this one predicate.

### Containment

Every ticket lives inside a container (`internal/storage/containment.go`). A leaf — `task`, `feature`, `bug`, `chore` — always has a parent, and every parent is an epic, so only an epic may be top-level and an epic may sit inside another epic. The rule is enforced where the hierarchy is written, in both engines, as four refusals, each a `model.ValidationError`: a create of a leaf with no parent (`storage.RejectLeafWithoutParent`), a create, reparent or parent-child edge naming a parent that is not an epic (`storage.RejectParentThatIsNotAContainer`), a detach that would leave a leaf with no parent (`storage.RejectDetachingALeaf`, asked of a removed edge through `storage.RejectDetachingParentEdge`) — detaching otherwise only lifts a nested epic to the top level — and a purge that would leave a leaf it holds with no parent (`storage.RejectPurgeOfOccupiedContainer`); a purge lifts a child epic to the top level. A type change across the container line is refused by the field patch (`internal/storage/fields.go`), so neither half can be broken by retyping an endpoint.

A write that lands an export whole has no single write to refuse, so a sync landing (`spineWriter.land`, `05-sync-merge-compaction.md`) and a restore (`ReplaceFromExport`, `04-store-operations.md`) pass what they land through `landExport` (`internal/store/rank_collisions.go`): first `storage.BreakParentCycles` (`internal/storage/parent_cycle.go`), which cuts every parent loop and returns the cuts as `[]storage.Unlooped{IssueID, Parent, Cycle}` in the order it made them, then `storage.HomeStranded` (`internal/storage/rehome.go`), which applies migration v7's rule and returns the moves as `[]storage.Rehomed{IssueID, Epic}`, sorted by id, then the separation of shared rank keys (`04-store-operations.md`); the three lists are returned together as a `storage.Landing{Unlooped, Rehomed, Rekeyed}` (`internal/storage/landing.go`). The cut (`storage.Unloop`, which a sync's lift of a historical commit also cuts by, `05-sync-merge-compaction.md`): `storage.ParentCycle`, the loop detector `lit doctor` reports with, names one loop's members in walk order; in it, the edge that goes is the one held by the member with the lexicographically greatest id, onto the next member of the loop; the child keeps any other parent it holds; the detector runs again until it finds no loop. An export with no loop is returned unchanged, with no cuts. A leaf the cut leaves with no parent is homed like any other. For homing, a ticket holding an epic parent stays, and loses any parent edge onto a ticket that is not an epic. Any other ticket with parents climbs them — as a set, through tickets that are not epics, stopping at an epic, a ticket with no parent, an id the export lacks, or depth 64 — and moves to the nearest epic reached, the lowest id at that depth. With no epic reached, or with an epic's climb reaching itself first (a loop through a ticket that is not an epic), an epic moves to the top level (`Epic` empty) and a leaf, as does a leaf with no parent at all, moves under the store's unfiled epic. A moved ticket's lane becomes its own id; each new edge carries the ticket's own `created_at` and `created_by` `links`; every parent-child edge onto a ticket that is not an epic is removed. Only the export's tickets are walked; an edge whose child the export lacks moves nothing. The unfiled epic takes tickets only while it is a live epic (`model.Frozen` false). It is the export's own — a live epic with topic `unfiled` and title `Unfiled tickets`, the lowest id if several — or else the first id in the prefix's chain that the export holds as a live epic or neither holds nor has purged, minted in the second case. The chain starts at `storage.UnfiledEpicID(prefix)`, `<prefix>-unfiled-` and the first four hex digits of SHA-256 over `<prefix>-unfiled`, and each next id is the same rendering over SHA-256 of the one before, so a purged, deleted or archived unfiled epic is walked past and every clone holding the same purge records reaches the same id. The prefix is the one the most top-level ids carry as `issueid.PrefixOf` reads them, ties to the lower prefix, `lit` when none reads. A minted row is the one migration v7 writes: title, topic and description as v7 writes them; `created_at` and `updated_at` `2026-10-12T00:00:00Z`; rank the highest-ranked live top-level leaf's key with `0` appended, else empty. An export with nothing stranded is returned unchanged, with no moves. Tests: `internal/storage/parent_cycle_test.go`; `TestRestoreCutsALoopTheExportHolds` (`internal/store/hierarchy_cycle_test.go`); `internal/storage/rehome_test.go`; `TestUnfiledEpicMatchesMigration00007` (`internal/store/rehome_test.go`) holds the minted row to the one v7 writes.

A recovery rebuild writes its source as it was (`rebuildFromExport`, `04-store-operations.md`), so a source from before the rule (migration v7, `05-sync-merge-compaction.md`) rebuilt that way is the one source of a leaf parent or a parentless leaf, and `lit doctor` warns on each (`04-store-operations.md`).

## Lanes

A **lane** partitions an epic's children into parallel, rank-ordered sub-sequences: children in the same lane are sequenced by rank (an earlier open sibling blocks a later one — see readiness in `06-issue-commands.md`); children in different lanes proceed in parallel (`model.go`). The empty string is the default lane, so an epic that declares no lanes is one fully-sequential lane, not a special case.

Lane identity is `(epic, lane-string)` — the same lane spelling under two different epics is two different lanes. An issue with no parent, or whose parent is not a container, is a "lane of one" keyed by its own ID (`model.go`) — a top-level epic, under containment; a leaf reaches that arm only from a recovery rebuild of data from before the rule. Lanes render as `epic#lane` (the default lane as `epic#`), a solo lane as the bare issue ID (`model.go`). The lane is the unit a checkout can claim (see `08-claims-and-identity.md`).

## Relations

A relation is a typed directed edge between two issues: `src_id`, `dst_id`, `type`, `created_at`, `created_by` (`model.go`). Three types (`internal/model/relation_type.go`):

| Type | Directionality | Multiplicity |
|---|---|---|
| `blocked-by` | directed | many-to-many |
| `parent-child` | directed | a child has at most one parent (`relation_type.go`); a leaf has exactly one, and every parent is an epic (`containment.go`) |
| `related-to` | undirected | many-to-many |

Two storage canonicalizations matter for anything reading rows directly:

- Every type reads as the sentence `<src> <type> <dst>`: `blocked-by` is stored **dependent → dependency** ("A blocked-by B": A waits for B), `parent-child` child → parent. The store, the export, and every command name an edge in that one order (`relation_type.go`).
- `related-to`, being undirected, is stored with its endpoints sorted ascending (`relation_type.go`).

Relation-type parsing trims but does not lowercase (`relation_type.go`).

## Comments

`id`, `issue_id`, `body`, `created_at`, `created_by` (`model.go`). Flat — no threading, no edits recorded as separate records.

## Labels

A label row is `(issue_id, name, created_at, created_by)` (`model.go`). Names are normalized to lowercase and trimmed; an empty result is rejected, and commas are forbidden because comma is the list separator on input surfaces (`internal/model/label.go`). There is no label registry — labels exist only as attachments to issues — and no label-rename operation exists anywhere in the store.

Two labels make an issue not-ready: `needs-design` and `external` (see readiness in `06-issue-commands.md`).

## Events (history)

Every mutation to an issue produces one **IssueEvent**: `id`, `issue_id`, `action` (the named transition verb for status/retention transitions, empty for plain field updates), `reason`, `actor`, `created_at`, `attribution`, and a list of field changes (`model.go`). Each **FieldChange** is `(field, from, to)` with both values stringified, so every field type lands in one schema shape (`model.go`). Per-field actions do not exist; one event covers all fields that moved together.

### Attribution

Attribution answers "which checkout produced this event": an opaque pair of a per-checkout **stream token** and the per-store **workspace id** (`model.go`). It is the entire shared-data footprint of the claims feature — claims are derived from these stamps at read time and stored nowhere (see `08-claims-and-identity.md`).

Rules enforced at every boundary (`model.go`):

- The pair is **complete or absent** — a stream without a workspace (or vice versa) collapses to unattributed, including when decoding JSON some other program wrote.
- Both halves are opaque by mandate: nothing user-, host-, or path-shaped is ever carried, because the database syncs to shared remotes.
- Attribution is append-only historical fact: written once at event creation, never rewritten, never backfilled. Events predating the feature are permanently unattributed — not missing data, but the work of the public checkout, the shared holder every unattributed write belongs to. An unattributed *establishing* event that is also a lane's latest one derives a claim `Held` by the public checkout exactly like any other holder's; other unattributed events (bare edits, non-establishing ones) never establish a claim on their own (see `08-claims-and-identity.md`).

## Identifiers

Top-level issue IDs have the shape `<prefix>-<topic>-<hash>` (`internal/issueid/generate.go`):

- **prefix** — the workspace's configured slug, 3–12 chars after normalization; over-long input is truncated to 12 then re-trimmed of dashes (`slug.go`).
- **topic** — a per-issue slug, 3–30 chars after normalization; over-long input is rejected, not truncated (`slug.go`).
- **hash** — lowercase base-36, adaptive length 3–8 chars.

Slug normalization lowercases, passes `a-z0-9` through, collapses every other rune (including Unicode) into a single dash, and trims edge dashes (`slug.go`).

The hash is deterministic content addressing: SHA-256 over `topic|title|description|creator|createdAt.UnixNano()|nonce` (the prefix is *not* hashed), truncated and base-36-encoded to exactly the chosen length (`generate.go`). Hash length adapts to a population count: the smallest length 3–8 whose birthday-bound collision probability stays ≤ 0.25 for that count, clamping at 8 (`generate.go`). The count is every top-level ID in the workspace for a top-level ID, and the parent's direct children for a child (`internal/store/issue_ids.go`). On collision, up to 10 nonces are tried (`generate.go`). Bytes map to characters with left-zero-padding and tail clamping (`generate.go`).

A child's ID is its parent's ID, a dot, and a hash minted the same way: `<parentID>.<hash>`, so grandchildren nest (`<parentID>.<hash>.<hash>`) (`internal/issueid/generate.go`).

## Ranking

Global ordering uses **lexicographic fractional indexing**: a rank is a string over the 62-character alphabet `0-9A-Za-z`, whose byte-wise string comparison *is* rank order (`internal/rank/rank.go`). Empty string means unranked and is never a stored rank value (`rank.go`).

- The first rank issued is `"V"` — the alphabet's midpoint (`rank.go`).
- `Midpoint(a, b)` returns a string strictly between two ranks, at the shortest length with room; either bound may be empty, meaning before-everything / after-everything, and both empty is the whole keyspace, whose midpoint is `"V"`, the initial rank (`rank.go`). Past an open end it steps one unit rather than bisecting, at the shortest power-of-two length from 4 that admits a step, so appends and prepends never deepen a corner. It refuses, with `ErrNoRoom`, a pair whose ranks are equal once trailing zeros are removed (`"10"` and `"100"`, or an empty lower bound and an all-zero upper one): nothing sorts between such a pair except further zero-extensions of the lower bound. It refuses a pair or a step with no room inside `MaxLength` the same way (`rank.go`).
- `SpacedRanks(n)` pre-allocates n evenly-spaced, fixed-width ranks at the shortest length with a gap of 16 code points between neighbors, sized to leave room for later midpoint insertion (`rank.go`).
- `MaxLength` is **190**, the longest key the package writes, one short of the 191-char prefix `idx_issues_rank` holds, so two keys it wrote cannot tie on that index (`rank.go`).
- A key that reaches its ceiling — **8 characters** in open keyspace, longer past a deep corner — and is longer than a neighbour a respace could move sets off a respace of the keys around it, starting from a window of **32** items (`rank.go`); the respace itself lives in the store (`03-store-schema.md`).

## Export format

`Export` is the interchange shape for sync files, backups, and `lit export`: `version`, `workspace_id`, `exported_at`, plus arrays of issues, relations, comments, labels, events, purges, and plan reviews (`model.go`). Current version is 5 (`ExportVersion`, `model.go`); the decoder reads `purges` and `plan_reviews` whatever the file's version, a file written before version 4 has no purges, and one written before version 5 has no plan reviews. Version 1 and 2 files spell the `blocked-by` type `blocks`; the decoder reads every relation type of a version < 3 file through `RelationTypeFromStored`, which maps `blocks` to `blocked-by` and passes every other spelling through (`model.go`, `relation_type.go`). Version 1 files carried a `history` array instead of `events`; the decoder converts each v1 history row into an event with a single `status` field-change and a deterministic content-derived ID (`evt-v1-` + 16 hex chars of SHA-256), so merging two v1 exports cannot mint duplicate IDs for different events (`model.go`). v2+ files' `history` arrays are ignored.

A **purge record** (`Purge`, `internal/model/purge.go`) is `issue_id`, `issue_created_at`, `purged_at`, `purged_by` — the ticket, named by its id and its `created_at`, and the act, never the issue's content. A record names an issue (`Purge.Names`) when the id is the same and the creation instants are equal, the same identity the merge's `Classify` uses; a different ticket under a purged id is not named by the record. `Purge.Key` is a record's identity: the id and the creation instant spelled in UTC RFC3339Nano, one spelling per instant, which is also the store's primary key. `UnionPurges` combines purge sets into one, one record per key, sorted by issue id and then `issue_created_at` ascending; when two sets each hold a record of one ticket, the earlier `purged_at` is kept, and at an equal instant the lexicographically lesser `purged_by` (`purge.go`). `Export.ApplyPurges(purges)` returns the export with `purges` as its purge set and with no row belonging to an issue a record names: the issue itself, every relation with a purged endpoint, and the purged issue's comments, labels, events, and plan reviews. Rows of other issues are untouched, so a description, `follows_up`, or redirect target that names a purged id keeps naming it (`purge.go`).

A **plan review** (`PlanReview`, `internal/model/plan_review.go`) is `id`, `issue_id`, `plan_digest`, `verdict`, `laws`, `created_at`, `created_by` — one reviewer's verdict on one version of an issue's `plan`. `plan_digest` (`PlanDigest`) is the hex SHA-256 of the plan text the review judged (`DigestPlan`), not the version number `lit history` shows it under: those numbers follow the history's order, and a sync that merges in an older edit renumbers the versions after it. `ParsePlanDigest` trims its input and admits only `^[0-9a-f]{64}$`, the lower-case spelling `DigestPlan` produces, refusing anything else with a `ValidationError`. `PlanReview.Judges(plan)` is true when the digest equals that text's digest, so a plan returned to a reviewed text is judged by that review again. `verdict` (`PlanVerdict`) is `pass` or `fail`; `ParsePlanVerdict` trims its input and refuses any other spelling with a `ValidationError`. `laws` are law tokens (`Law`); `ParseLaw` trims its input and admits only `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, refusing anything else with a `ValidationError`, so no token holds a comma; lit holds no list of laws, so a token is checked for shape only. `CanonicalLaws` sorts the tokens and keeps each once — the order every review carries — and a `fail` cites at least one (`plan_review.go`).

## Serialization boundary rules

Behaviors any reimplementation must preserve at the JSON boundary (`model.go`):

- A leaf issue on the wire always carries `status`; a leaf without one fails to decode. An epic never carries status keys, and a decoded epic cannot be used for state reads until the store re-derives its lifecycle from children.
- `archived_at`/`deleted_at` are projections of the retention axis (deletion wins on decode if both are present).
- Unattributed events omit the `attribution` key entirely rather than writing an empty object.
- In-memory, unhydrated lifecycle reads are programmer errors and panic; the JSON boundary and the action-dispatch path convert the same condition to errors instead (`model.go`).
