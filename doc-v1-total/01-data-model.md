# Data model

lit's unit of work is the **issue**. Epics are not a separate record type: an epic is an issue whose type is `epic`, and its open/closed state is computed from its children at read time, never stored. Around the issue sit six persisted record kinds — relations, comments, labels, events, event field-changes, and workspace metadata — plus several derived (never-stored) concepts: epic state, lanes, claims, and readiness annotations.

This document covers the records and the value vocabularies they use. Where a rule has edge cases, the exact behavior is stated; `file:line` citations point at the defining source.

## The Issue record

An issue carries these persisted fields (`internal/model/model.go:80-111`):

| Field | Type | Notes |
|---|---|---|
| `id` | string | `<prefix>-<topic>-<hash>`; see [Identifiers](#identifiers) |
| `title` | string | |
| `description` | string | |
| `prompt` | string | optional free-text working instructions; omitted from JSON when empty |
| `priority` | int | `0` = normal, `1` = urgent; the only two values |
| `issue_type` | string | `task`, `feature`, `bug`, `chore`, or `epic` |
| `topic` | string | slug embedded in the ID; see [Identifiers](#identifiers) |
| `assignee` | string | owner identity; orthogonal to status and preserved across every transition (`model.go:88-91`) |
| `rank` | string | fractional-index ordering key; `""` means unranked; see [Ranking](#ranking) |
| `lane` | string | partitions an epic's children into parallel sequences; see [Lanes](#lanes) |
| `labels` | []string | normalized lowercase; see [Labels](#labels) |
| `created_at`, `updated_at` | timestamp | |
| status fields | | `status`, `closed_at`, `resolution`, `redirect_target` — present on the wire only for non-epics; see [Status lifecycle](#the-status-lifecycle) |
| retention fields | | `archived_at`, `deleted_at` — the wire encoding of the retention axis; see [Retention](#the-retention-axis) |

There is no stored `progress` field; an epic's progress counts are computed (`model.go:556-562`).

### Issue types

Five types (`internal/model/issue_type.go:17-23`): `task`, `feature`, `bug`, `chore`, `epic`. Exactly one, `epic`, is a **container** (`issue_type.go:56-58`). Container-ness is decided by the type alone, never by whether the issue has children (`model.go:369-371`). Type parsing lowercases and trims; anything outside the five values is rejected (`issue_type.go:42-50`).

Containers differ from leaves in three ways:

1. **Derived state.** An epic's state is computed from its children: all children closed (and at least one child) → `closed`; any child in progress or closed → `in_progress`; otherwise → `open`. An epic with no children is `open` (`internal/model/lifecycle/all_of.go:17-27`). Progress is the field-wise sum of every non-container descendant's counts (`all_of.go:29-39`).
2. **No direct transitions.** Applying any status action to an epic returns a typed `ContainerActionError` whose message varies with child progress — no children, all done, or N unfinished (`model.go:272-295, 310-324`).
3. **No status on the wire.** A serialized epic carries no `status`, `closed_at`, `resolution`, or `redirect_target` keys (`model.go:501-511`), and JSON alone cannot reconstruct an epic's lifecycle — a decoded epic is marked pending until the store re-derives its state from children (`model.go:558-561`).

### Priorities

Two values (`internal/model/priority.go:14-17`): `0` (normal) and `1` (urgent). `CanonicalPriority` maps every other integer — negative or ≥2 — to normal (`priority.go:25-30`); the strict parser accepts only 0 and 1 (`priority.go:38-44`).

## The status lifecycle

A leaf issue's status is one of three states (`internal/model/lifecycle/lifecycle.go:20-24`):

- `open`
- `in_progress`
- `closed`

State parsing lowercases, trims, and accepts `in-progress` as an alias for `in_progress` (`lifecycle.go:98-109`). Lenient boundaries (import, hydration, storage) default unparseable states to `open`; strict boundaries (CLI flags, query language) reject them (`lifecycle.go:111-121`).

### Transition actions

Eight named actions, split across two independent axes (`lifecycle.go:46-58`):

| Action | Axis | Effect |
|---|---|---|
| `start` | status | → `in_progress`; the only action that carries and rewrites the assignee (`action.go:44-49`) |
| `done` | status | → `closed` with **no** resolution (the neutral success close) |
| `close` | status | → `closed` with a mandatory outcome (resolution) |
| `reopen` | status | → `open`; clears `closed_at`, resolution, and redirect target (`status_states.go:159-160`) |
| `archive` | retention | live → archived |
| `unarchive` | retention | archived → live |
| `delete` | retention | live or archived → deleted |
| `restore` | retention | deleted → live |

Status transitions are **target-state**, not edge-guarded: an action names the destination state, and applying it from any state succeeds. There is no enforced precondition (e.g. `done` does not require `in_progress` — see the discrepancy note in `06-issue-commands.md`). Applying an action whose target equals the current state returns the issue unchanged — in particular, re-closing a closed issue preserves its existing resolution and `closed_at` rather than rewriting them (`status_states.go:148-152`). Transitioning into `closed` stamps `closed_at = now (UTC)` (`status_states.go:154-156`).

The type system separates the two axes: retention actions are not status actions, so applying `archive` to the status machine is unrepresentable rather than checked (`action.go:23-42`).

### Resolutions and redirects

`close` requires an **outcome**; `done` records none. Four resolutions (`internal/model/lifecycle/resolution.go:22-27`):

| Resolution | Kind | Payload |
|---|---|---|
| `duplicate` | redirecting | `of <canonical-ticket>` — the redirect target |
| `superseded` | redirecting | `by <replacing-ticket>` — the redirect target |
| `obsolete` | terminal | none |
| `wontfix` | terminal | none |

A **redirect target** — the ticket the work went to instead — can exist only on a closed issue whose resolution is `duplicate` or `superseded`; the constructor drops a target supplied beside a terminal or absent resolution (`status_states.go:121-133`). Targets are normalized: trimmed, with blank collapsing to absent (`status_states.go:211-220`). At the type level a closed issue *may* lack `closed_at`, resolution, or (for redirecting closes) the target — legacy rows and field-wise merges can produce those — but every new `close` via the CLI requires an outcome, and new redirecting closes require a target (`status_states.go:78-94`).

Resolution parsing trims but does **not** lowercase (`resolution.go:47-54`), unlike state and type parsing.

## The retention axis

Retention is a second lifecycle axis, orthogonal to status: `live`, `archived`, or `deleted` (`internal/model/lifecycle/retention.go:19-35`). Archived-and-deleted simultaneously is unrepresentable. On the wire and in storage the axis is encoded as two nullable timestamps, `archived_at` and `deleted_at`; decoding gives deletion precedence, so a legacy row with both set reads as deleted (`retention.go:120-129`).

- **Archived**: hidden from default listings but still occupies rank space; reversible via `unarchive`.
- **Deleted**: hidden from default listings and excluded from rank space; reversible via `restore`.

The complete transition table (`retention.go:64-111`):

| current \ action | archive | unarchive | delete | restore |
|---|---|---|---|---|
| live | → archived | error | → deleted | error |
| archived | error | → live | → deleted | error |
| deleted | error | error | error | → live |

Deleting an archived issue drops the archive stamp, so a later `restore` always lands on `live`, never back on `archived` (`retention.go:87-91`).

An issue that is archived or deleted is **frozen**. The single program-wide definition of "still unfinished work" is `InPlay`: not frozen and not closed (`model.go:165-167`). Readiness gating and claim derivation both read this one predicate.

## Lanes

A **lane** partitions an epic's children into parallel, rank-ordered sub-sequences: children in the same lane are sequenced by rank (an earlier open sibling blocks a later one — see readiness in `06-issue-commands.md`); children in different lanes proceed in parallel (`model.go:93-97`). The empty string is the default lane, so an epic that declares no lanes is one fully-sequential lane, not a special case.

Lane identity is `(epic, lane-string)` — the same lane spelling under two different epics is two different lanes. An issue with no parent, or whose parent is not a container, is a "lane of one" keyed by its own ID (`model.go:212-217`). Lanes render as `epic#lane` (the default lane as `epic#`), a solo lane as the bare issue ID (`model.go:228-233`). The lane is the unit a checkout can claim (see `08-claims-and-identity.md`).

## Relations

A relation is a typed directed edge between two issues: `src_id`, `dst_id`, `type`, `created_at`, `created_by` (`model.go:579-585`). Three types (`internal/model/relation_type.go:16-20`):

| Type | Directionality | Multiplicity |
|---|---|---|
| `blocks` | directed | many-to-many |
| `parent-child` | directed | a child has at most one parent (`relation_type.go:54-56`) |
| `related-to` | undirected | many-to-many |

Two storage canonicalizations matter for anything reading rows directly:

- `blocks` is stored **dependent → dependency** — the reverse of the human reading "X blocks Y". The swap is an involution, so the same conversion maps store order back to display order (`relation_type.go:35-45`).
- `related-to`, being undirected, is stored with its endpoints sorted ascending (`relation_type.go:62-67`).

Relation-type parsing trims but does not lowercase (`relation_type.go:26-33`).

## Comments

`id`, `issue_id`, `body`, `created_at`, `created_by` (`model.go:587-593`). Flat — no threading, no edits recorded as separate records.

## Labels

A label row is `(issue_id, name, created_at, created_by)` (`model.go:595-600`). Names are normalized to lowercase and trimmed; an empty result is rejected, and commas are forbidden because comma is the list separator on input surfaces (`internal/model/label.go:14-23`). There is no label registry — labels exist only as attachments to issues — and no label-rename operation exists anywhere in the store.

One label has behavioral meaning: `needs-design` makes an issue not-ready (see readiness in `06-issue-commands.md`).

## Events (history)

Every mutation to an issue produces one **IssueEvent**: `id`, `issue_id`, `action` (the named transition verb for status/retention transitions, empty for plain field updates), `reason`, `actor`, `created_at`, `attribution`, and a list of field changes (`model.go:718-733`). Each **FieldChange** is `(field, from, to)` with both values stringified, so every field type lands in one schema shape (`model.go:602-610`). Per-field actions do not exist; one event covers all fields that moved together.

### Attribution

Attribution answers "which checkout produced this event": an opaque pair of a per-checkout **stream token** and the per-store **workspace id** (`model.go:631-634`). It is the entire shared-data footprint of the claims feature — claims are derived from these stamps at read time and stored nowhere (see `08-claims-and-identity.md`).

Rules enforced at every boundary (`model.go:656-713`):

- The pair is **complete or absent** — a stream without a workspace (or vice versa) collapses to unattributed, including when decoding JSON some other program wrote.
- Both halves are opaque by mandate: nothing user-, host-, or path-shaped is ever carried, because the database syncs to shared remotes.
- Attribution is append-only historical fact: written once at event creation, never rewritten, never backfilled. Events predating the feature are permanently unattributed, which reads as "derives no claim", not as missing data.

## Identifiers

Issue IDs have the shape `<prefix>-<topic>-<hash>` (`internal/issueid/generate.go:42-47`):

- **prefix** — the workspace's configured slug, 3–12 chars after normalization; over-long input is truncated to 12 then re-trimmed of dashes (`slug.go:31-45`).
- **topic** — a per-issue slug, 3–30 chars after normalization; over-long input is rejected, not truncated (`slug.go:47-59`).
- **hash** — lowercase base-36, adaptive length 3–8 chars.

Slug normalization lowercases, passes `a-z0-9` through, collapses every other rune (including Unicode) into a single dash, and trims edge dashes (`slug.go:15-29`).

The hash is deterministic content addressing: SHA-256 over `topic|title|description|creator|createdAt.UnixNano()|nonce` (the prefix is *not* hashed), truncated and base-36-encoded to exactly the chosen length (`generate.go:42-47`). Hash length adapts to workspace size: the smallest length 3–8 whose birthday-bound collision probability stays ≤ 0.25 for the current issue count, clamping at 8 (`generate.go:22-36`). On collision, up to 10 nonces are tried (`generate.go:12-18`). Bytes map to characters with left-zero-padding and tail clamping (`generate.go:49-86`).

Children created under a parent may instead get sequential `parent.N` IDs (see the storage layer, `02-storage-contract.md`).

## Ranking

Global ordering uses **lexicographic fractional indexing**: a rank is a string over the 62-character alphabet `0-9A-Za-z`, whose byte-wise string comparison *is* rank order (`internal/rank/rank.go:17-36`). Empty string means unranked and is never a stored rank value (`rank.go:53-63`).

- The first rank issued is `"V"` — the alphabet's midpoint (`rank.go:39-41`).
- `Midpoint(a, b)` returns a string strictly between two ranks; either bound may be empty, meaning before-everything / after-everything (`rank.go:69-117`). Between adjacent characters the result grows one character longer, so insertion between any two ranks always succeeds without renumbering neighbors.
- `SpacedRanks(n)` pre-allocates n evenly-spaced, fixed-width ranks with a minimum gap of 16 code points between neighbors, sized to leave room for later midpoint insertion (`rank.go:129-196`).
- Rank strings reaching **8 characters** trigger local smoothing over a window of **32** items (`rank.go:120-126`); the smoothing operation itself lives in the store (`03-store-schema.md`).

## Export format

`Export` is the interchange shape for sync files, backups, and `lit export`: `version`, `workspace_id`, `exported_at`, plus arrays of issues, relations, comments, labels, and events (`model.go:755-764`). Current version is 2. Version 1 files carried a `history` array instead of `events`; the decoder converts each v1 history row into an event with a single `status` field-change and a deterministic content-derived ID (`evt-v1-` + 16 hex chars of SHA-256), so merging two v1 exports cannot mint duplicate IDs for different events (`model.go:766-836`). v2+ files' `history` arrays are ignored.

## Serialization boundary rules

Behaviors any reimplementation must preserve at the JSON boundary (`model.go:488-577`):

- A leaf issue on the wire always carries `status`; a leaf without one fails to decode. An epic never carries status keys, and a decoded epic cannot be used for state reads until the store re-derives its lifecycle from children.
- `archived_at`/`deleted_at` are projections of the retention axis (deletion wins on decode if both are present).
- Unattributed events omit the `attribution` key entirely rather than writing an empty object.
- In-memory, unhydrated lifecycle reads are programmer errors and panic; the JSON boundary and the action-dispatch path convert the same condition to errors instead (`model.go:142-149, 488-496`).
