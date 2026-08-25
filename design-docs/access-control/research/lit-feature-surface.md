# Research: lit feature-surface inventory

Evidence base for [../design.md](../design.md), covering charter process steps
2 (feature survey) and feeding 3 (forward constraints). First compiled
2026-08-25 from source. This document describes lit's *behavior* — the
features and contracts an access-control layer must cover — not its
implementation, so it stays true across refactoring. References are to
concepts and modules (`internal/<pkg>`), with a named file only where that
file is itself the artifact being discussed. When this document and the code
disagree, the code wins; correct the document.

Two framing facts up front:

- The store lives inside the repository's private git directory (under the
  git common dir, shared by all worktrees of a clone), never in the worktree.
  Nothing in it is a tracked file. Data reaches remotes only as Dolt content
  pushed to the `refs/dolt/*` namespace of the code repo's own git remote.
- There is **no auth layer of any kind** today. The only gate any command
  passes is its declared access mode — read vs write — which selects a
  store-open contract and whether a checkout identity is minted. Access mode
  is a concurrency/identity contract, not a permission.

## 1. Command surface

Commands are registered declaratively (one table in `internal/cli`), each row
carrying its access mode — which means the future capability-per-verb mapping
has a natural single home. The inventory below is the *feature* list; flags
shown are the semantically load-bearing ones, not an exhaustive reference.

R = reads store, W = writes store (one Dolt commit per mutation), **–** =
never opens the store.

### Bootstrap

| Command | Behavior | Store |
|---|---|---|
| `init` | Creates the store — or, when the remote already advertises lit data, adopts the remote backlog wholesale instead of creating an empty one. Also installs the pre-push hook and writes managed sections into the repo's agent-guidance files. A half-finished adoption leaves a marker that refuses every later store open until resolved. | W (creates/replaces whole DB) |

### Ticket operations

| Command | Behavior | Store |
|---|---|---|
| `new` | Create an issue: title, description, agent prompt, type, topic, parent, priority, assignee, labels, lane; placement defaults to bottom-of-frame, `--top` opts out | W |
| `followup` | Create a child of a just-closed ticket, inheriting its topic; the default description quotes the parent's title | W |
| `backlog` / `next` | The ranked workable queue (all of it / one leaf), with blocked items annotated inline; honors project-configured required fields | R |
| `orphaned` | In-progress issues stale past a threshold | R |
| `ls` | General query: filter by status/type/assignee/labels/dates/free-text search, project columns, sort, limit; `--at <dir>` opens *another project's* store by path | R |
| `show` | Full detail: fields, relations in both directions, children, siblings, comments, event history | R |
| `history` | Per-field from→to change trail for one issue | R |
| `update` | Field writes (title, description, prompt, type, priority, assignee, labels, lane); status changes are rejected here and routed to the lifecycle verbs | W |
| `rank` | Move one issue (top/bottom/above/below) or set an absolute N-way order atomically | W |
| `start` / `done` / `close` / `open` | The lifecycle verbs: claim into in_progress, close as finished, close unfinished with a resolution (duplicate/superseded/obsolete/wontfix, optionally redirecting to another id), reopen | W |
| `comment add` / `comment rm` | Comments are first-class rows with their own ids | W |
| `label add/rm`, `bulk label/close/archive` | Label edits and bulk fan-out of labels, closes, archives | W |

### Structure

Parent-child edges (`parent set/clear`, `children`), and typed dependency
edges (`dep add/rm/ls`) with a closed relation-type set: blocks,
parent-child, related-to.

### Data

| Command | Behavior | Store |
|---|---|---|
| `export` | The whole backlog as one JSON document on stdout — every issue, relation, comment, label, and event | R |
| `import` | Bulk ingest: a tree spec that creates whole subtrees, or per-issue documents that create-or-update by id | W |
| `backup create/list/restore` | Rotating on-disk JSON exports; restore replaces the entire store from a chosen file | R / R / W |
| `snapshots new/list/restore` | Filesystem-level copies of the whole database directory; the system also stamps its own recovery snapshots before migrations, downgrades, and reconciles | filesystem |
| `sync …` | Mirroring to/from the git remote — see §4 | R/W |

### Retention

`archive`/`unarchive` and `delete`/`restore` set and clear timestamps on a
surviving row. Deletion is soft: the row, its id, its rank, and its relations
remain in the database and are recoverable.

### Maintenance

`workspace` (prints the store's locations and ids without opening it),
`stores` (discovers lit stores under filesystem roots and can open each
read-only for counts), `prefix set` (cosmetic id prefix), `doctor` (health
report; `--fix` repairs integrity and rank inversions — its access mode is
chosen at runtime from the flag), `hooks install`, `lifeboat dump/recover`
(disaster path — see §5), `upgrade`/`downgrade` (binary/schema version
management; downgrade runs down-migrations and swaps the installed binary).

### Guidance

`quickstart` (renders workflow instructions; `--eject` copies templates for
user override), `workflows` (user-authored guidance injected into command
output at lifecycle moments — see §6), `completion`, `version`. None touch
ticket data.

### Retired commands

A handful of retired names (`ready`, `queue`, `assign`, others) remain
dispatchable but only print a pointer to their replacement.

## 2. Data model

Authoritative schema: `internal/store/schema_snapshot.sql` — generated, and
byte-compared by a drift canary, so schema change is deliberately loud. Seven
application tables. What matters for access control is each field's *kind*:
free text a tier could encrypt, versus structure the whole workspace must
read.

**Issues** carry: an id; three free-text fields (title, description, agent
prompt); a topic slug; and structural fields — status, priority, type, lane,
a global fractional rank, four lifecycle timestamps plus archived/deleted
retention timestamps, a close resolution, and an optional redirect target.
Database CHECK constraints enforce the semantic couplings (epics have no
status of their own; a redirect target requires a redirecting resolution).

**The issue id is not opaque.** A top-level id embeds the plaintext topic
slug and a hash computed over topic, title, description, creator, and
creation time — a commitment to the content. A child id is its parent's id
plus a sequence number, so ids alone reveal parentage and sibling count. Ids
appear everywhere: every output, every relation row, every trace, and in
branch names derived from tickets.

**Comments are rows, not a field** — each with its own id, body (free text),
creation time, and creator. **Labels** are rows keyed (issue, label); label
names are a queryable dimension. **Relations** are rows keyed (src, dst,
type) over the closed type set; parentage is therefore recorded twice — in
the child's id string and as a relation row — plus enforced by cascade.

**Events are the history and attribution spine.** Every mutation records an
event (action, free-text reason, actor, timestamp, and the attribution
fields of §3) through a single store-internal recording point, and a
companion change table stores the stringified old and new value of every
changed field — meaning **the free-text history of title/description/prompt
is duplicated into the event tables in plaintext**.

**Foreign keys cascade.** Deleting an issue row cascades away its comments,
labels, relations, and events. Soft delete never triggers this; the sync
delta machinery does (§4).

**A meta table** holds the workspace id, producer version, and the last-sync
bookkeeping — which today includes a **local filesystem path stored in the
synced database**, an existing privacy leak independent of this design.

**One wire form.** A single export structure (all issues, relations,
comments, labels, events, plus the workspace id) is simultaneously the
`export` command's output, the backup file, the sync merge base, the restore
input, and the unit the three-way merge operates on. Changing how data is
represented at rest changes all of those at once — a fact the design relies
on (encrypt once, protected everywhere).

**Field kinds, summarized:**

- *Free text (ciphertext candidates):* issue title, description, agent
  prompt; comment body; event reason; event-change old/new values.
- *Identity-bearing:* assignee, comment/label/relation creator, event actor.
- *Structural (plaintext by necessity today):* ids (and therefore topic and
  parentage), rank, lane, status, priority, type, timestamps, resolution,
  redirect target, all relations, label names, event action, changed-field
  names, attribution ids.

## 3. Identity & attribution

Three deliberately layered identity notions (normative source:
`design-docs/work-claims.md`).

**(a) Stream id — per-checkout, opaque, local-lifecycle.** Each checkout
(primary clone or linked worktree) mints an opaque random token on its first
mutating command, stored write-once in that checkout's private git
directory — so git itself creates and destroys the identity with the
checkout, and all worktrees of one clone share a store while each carries its
own stream. Publication is atomic and never-overwriting: concurrent first
mutations race safely, the loser adopts the winner's token, and an existing
identity is never replaced. A damaged token fails every command in that
checkout loudly and is never silently healed (a quiet replacement would give
one checkout two identities). Read-only commands in a never-mutated checkout
mint nothing — enforced structurally by the access-mode declaration, not by
convention.

**(b) Workspace id — per-store, opaque, NOT synced.** A UUID in the store's
local config. It does not travel with the data — a clone mints its own — but
it *is* stamped into synced rows: every event carries the workspace id it
was written under, and it names the export. Multiple workspace ids therefore
coexist in a synced history.

**(c) The attribution pair.** Every event carries (stream, workspace) as one
sealed value: a half-pair collapses to none, the pair is attached at a
single store-internal point so no call site can forget it, and pre-feature
events are never backfilled — they permanently derive no claim.

**(d) Claim derivation is pure and stores nothing.** `internal/claims`
derives lane claims from the evidence (issues partitioned into lanes, events
filed by lane) at read time. Only the start and done actions establish a
claim; everything else merely refreshes one, within a configurable freshness
window. Evidence over a *local* workspace is additionally pruned against the
live set of local checkouts, so a deleted worktree's claims vanish
immediately here and age out elsewhere. Two load-bearing properties: the
evidence constructor **hard-fails when events reference issues it wasn't
given** — a deliberate guard so a partial read can never produce a wrong
claim — and derivation reads only structural fields, never free text.

**Status flag:** as of first writing, derivation exists and is exercised by
tests but is not yet consulted by `backlog`/`next` routing. Verify current
wiring before designing against it.

**The privacy invariant (normative).** Only opaque discriminators may enter
the shared database — no hostnames, usernames, paths, or device details;
resolution to physical context happens at render time on the owning machine.
**Actor identity violates this today**: by documented convention the
assignee/actor/creator fields carry `<tool>_<sessionId>` for agents and raw
usernames for humans, and `ls --assignee` filters on those strings. The
claims design itself flags these fields for review; the access-control
design resolves them into key-bound opaque principals.

## 4. Sync & history model

**One Dolt commit per mutation**, under a kernel-flock commit lock, so the
store's own history is a complete, ordered, per-mutation audit trail with
attribution. This is the substrate the write layer signs.

**Transport is the code repo's own git remote.** Before every sync
operation, lit re-derives its Dolt remotes from `git remote -v`, so git
remotes are the single remote configuration. Data lands under `refs/dolt/*`;
the presence of that namespace is the authoritative "this remote carries a
backlog" signal, and drives `init`'s adopt path (a fresh store has an
unrelated root, so first contact is adoption, not pull).

**Sync commands** (`status`, `remote ls`, `fetch`, `pull`, `push`,
`reconcile`) report their outcome as a closed state set (up-to-date,
fast-forwarded, linearized, prose-pending, unrelated-histories, ahead,
never-synced).

**Linear history via reconcile.** Divergence is resolved not by a merge
commit but by *linearization*: lit three-way merges the two exports against
their merge base, then replays the local commits one-per-original onto the
remote head — each replayed commit keeping its original message, date, and
authorship, its content re-derived as a delta. The data branch moves only in
one atomic step at the end; failure leaves it untouched. **Reconcile is
therefore a history rewrite**: replayed commits sit on new parents with
projected content. Any signing scheme must treat "authored" and "faithfully
replayed" as distinct, verifiable facts.

**The merge is field-aware, with one refusal.** Concurrent edits resolve
per-field: single-side change wins; both-sides changes fall to per-field
policy (higher priority wins, labels union, deletion dominates retention,
status joins toward closed, close payload moves as an atom, symmetric
tiebreaks for the rest). The engine refuses exactly one thing: **concurrent
free-text divergence** (title, description, prompt). That surfaces as a
pending-prose conflict carrying base/ours/theirs plaintext out to the
*calling agent*, who supplies merged text back, pinned by a fingerprint over
the three versions; the resolution set must exactly match the pending set or
the whole batch is rejected. Destructive escapes (take local / take remote)
exist behind an owner-approval token. Nothing commits while prose is
pending.

**Push cadence is a config policy.** Default `on-change`: every mutating
command spawns a short-lived detached mirror process that pushes after the
command exits, with coalescing so bursts run a few mirrors rather than one
per mutation. Alternative `on-push`: only the managed git pre-push hook
mirrors. A debounced background *receive* similarly fast-forwards from the
remote around command activity. Both are unattended processes that open the
store and touch the network with no user present. A kill switch env var
disables all sync automation. Every sync/init decision — interactive or
automated — is durably recorded as a JSON trace (§5).

**Whole-store replacement paths** (beyond reconcile): restore-from-backup,
reset-to-remote, adopt, the lifeboat's rebuild-and-promote, downgrade's
down-migrations, and schema migrations generally (which run under an
automatic pre-migration snapshot guard, with failed migrations quarantined).
Migrations and the legacy-schema reconciler read and rewrite entire tables —
they are exactly the kind of code that must remain correct when free-text
columns hold opaque envelopes.

**Flagged observation (verify before relying on it):** the merge's
symmetric tiebreak is specified to compare the two sides' workspace ids, but
the reconcile path appears to hand both sides the *local* id, so the
tiebreak decays to a value comparison. Possibly benign (the value comparison
is also symmetric), possibly latent. Behavior-level statement: **the
tiebreak's determinism must not be assumed to derive from workspace
identity.**

## 5. Escape hatches & side channels

Every path where ticket data leaves the store's normal read/write flow.
Ranked roughly by how completely plaintext escapes.

1. **`export`** — the whole database as JSON on stdout; plain read access,
   no redaction, no filter.
2. **`lifeboat dump`** — the whole database, raw, **below the migration
   gate**: it deliberately works on stores the normal open path refuses, and
   deliberately has no notion of what any column means. Its recovery
   counterpart rebuilds a store from a dump through an agent-authored
   mapping, verified by conservation checks. The hardest surface to police,
   by design.
3. **`backup create`** — full plaintext export written to disk under *read*
   access; plaintext egress is not currently treated as a privileged act.
4. **`backup restore`** — ingests any export file, provenance-blind by
   design, replacing the whole store; also leaves a second full plaintext
   copy on disk as the new sync base.
5. **Snapshots** — byte copies of the whole database directory, user- and
   system-initiated.
6. **`import`** — bulk create-or-update by id, bypassing the interactive
   verbs; a single command can rewrite arbitrary issues.
7. **Foreign-store opens** — `ls --at` and `stores --counts` open *other
   projects'* stores read-only by filesystem path, with no workspace context
   of their own and no per-project policy consulted. Cross-project access
   needs an explicit story.
8. **Trace files** — sync and workflow decision traces under the store
   directory carry command context, decision reasons, error strings, issue
   ids, and labels in plaintext on local disk (never synced).
9. **Owner-notify hook** — a user-configured shell command invoked with
   event summaries in its environment: an arbitrary egress channel by
   design.
10. **The managed pre-push hook** — mirrors on every git push and prints
    failure context to the pushing terminal.
11. **The synced-path leak** — the last-sync bookkeeping stores a local
    filesystem path inside the synced database (§2), violating the privacy
    invariant today.
12. **Issue ids** — carry topic and parentage everywhere output goes (§2).

## 6. Extension points / config

**Config** is layered TOML: a global file in the user's config directory and
a per-project file at `.lit/config.toml` — note the per-project file lives
in the *worktree*, so it is a tracked, shared, plaintext file even though the
store is not. Policy-relevant keys exist for sync cadence, background
receive, the owner-notify command, migration auto-apply, backlog
required-fields, snapshot retention, and the claims freshness window. Env
vars override config paths, disable sync automation, and (today) supply
identity via the calling tool's session id.

**Workflows** — user-authored markdown guidance *injected as text, never
executed* into agent-facing output at a closed catalog of lifecycle moments
(backlog shown, ticket shown/created/updated/closed/reopened, work
started/finished, comment added), activated by labels, states, or events;
definitions layer project-over-global-over-embedded, nearest wins.

**Templates** — the quickstart texts, agent-guidance sections, and the
pre-push hook are embedded defaults, ejectable to the user's config dir for
override; `init`/`quickstart --refresh` rewrite only their marker-delimited
managed sections of repo files.

**Signalled future directions** (the only committed statements found): a
possible bidirectional Jira bridge (`design-docs/project-intent.md` —
explicitly uncommitted); opt-in user-chosen friendly labels for checkout
streams (a place where non-opaque data would deliberately enter the shared
DB unless routed through keyring metadata); a deferred process-liveness
probe. The project's stated bias is toward *fewer* features; note the
design-docs convention that code wins over docs — `work-claims.md`'s own
status header is the exception, claiming a divergence from it is a bug in
one of them rather than an automatic win for either.

## 7. Strain points for an access-control layer

Each is a *behavioral contract that holds today* and would interact with
row-filtering or ciphertext. Stated as invariants so they survive refactors.

**7.1 Claims refuse partial evidence.** Claim derivation requires the
complete issue set and hard-fails on events referencing issues it wasn't
given — a guard ensuring a partial read can never yield a wrong claim. A
row-filtering RBAC layer *is* a partial read and will hit the guard, not
degrade gracefully. Compatible resolution: derivation reads only structural
fields, so it can run over the full row set with free text still encrypted —
but that must become a stated law, not an accident.

**7.2 Prose conflicts are resolved by an agent holding plaintext.** The
reconcile hands base/ours/theirs plaintext to the calling agent and pins the
answer with a fingerprint over those plaintexts; an incomplete resolution
set rejects the entire batch. Under encryption: only keyholders can
reconcile a tier, fingerprints must be redefined over ciphertext, and the
all-or-nothing batch rule must scope per tier or one unreadable field blocks
everyone's reconcile.

**7.3 The merge unit is the whole export, and absence means deletion.** The
three-way merge walks every issue across both sides, and the delta
machinery interprets a row missing from the next state as a deletion to
cascade. A client that cannot read some rows must still carry them through
byte-faithfully — "unreadable" must be representable as distinct from
"absent" — or merging destroys data silently.

**7.4 Rank is one global order.** Rank operations read and rewrite
neighbors across the entire backlog (top/bottom queries, N-way set,
inversion repair), and an epic's rank carries its children. "Rank to top"
is meaningless if the top is invisible; therefore ticket *existence and
rank* must stay readable to every member (the design's visible-skeleton
rule) or ranking fragments per tier.

**7.5 The queue needs titles, and filters run in SQL.** Backlog/next render
titles; search, assignee, and label filters are database predicates over
plaintext columns. Encrypted fields move filtering to the client after
decryption; anything that must stay a database predicate must stay
structural plaintext.

**7.6 Structure crosses tiers by construction.** Parentage lives in the id
string, the relations table, and cascades; lane readiness is computed over
an epic's *full* child set (a hidden sibling still gates its lane-mates);
an epic's own status is derived from all of its children. Content-hiding
survives all of this; existence-hiding breaks all of it at once.

**7.7 Cascade rewrite cycles must preserve envelopes.** Sync deltas express
a changed issue as delete-plus-reinsert, cascading child rows away and back.
Whatever encryption metadata rides on rows must survive that round trip
bit-faithfully, and cascade accounting must never read "couldn't decrypt"
as "should be dropped" (same invariant as 7.3, at the row-lifecycle level).

**7.8 Recovery verification leans on distinguishable plaintext.** The
lifeboat's conservation checks compare rebuilt stores against raw dumps and
already acknowledge that same-typed free-text fields can be swapped
undetectably. Uniform ciphertext makes *all* encrypted fields same-typed;
the verification gate needs envelope-integrity checks (tier/epoch/nonce
round-trip) to compensate.

**7.9 The lifeboat is a deliberate below-the-gate bypass.** It reads the
store without the migration gate and without schema knowledge, on purpose —
any enforcement living above that gate does not cover it. The clean answer
is representational: if the at-rest form is the protected form (ciphertext
in the columns), the lifeboat needs no exemption because what it dumps is
already protected.

**7.10 Actor identity is human-readable in the synced DB today.** Session
ids and usernames sit in assignee/actor/creator fields, contradicting the
opaque-discriminator invariant, and are filtered on by commands. Binding
capabilities to identity forces the choice: opaque key-bound principals
(consistent with the invariant, requires display-time resolution) or the
status quo (leaks who works on what to every repo reader).

**7.11 Foreign-store reads bypass workspace context.** The by-path open
used by `ls --at` and `stores` consults no policy and carries no identity.
Under access control these paths need keys and policy from the *target*
store or they become either broken or a bypass.

**7.12 Unattended processes need credentials.** The on-change mirror and
background receive run detached, after the invoking command exits, with no
user present. Signing keys and tier keys must be available non-interactively
(private git dir / OS keychain), or sync automation dies — a hard
transparency constraint.

**7.13 The existing owner-approval gate is advisory.** The destructive
reconcile escapes require an approval token — but the same client computes
the token, so it is a speed bump for honest agents, not enforcement. It is
the in-repo precedent for "owner sign-off on destructive acts" and precisely
the pattern the write layer upgrades to a real signature.

**7.14 The schema is deliberately hard to change quietly.** CHECK
constraints couple structural fields; a generated schema snapshot is
byte-compared as a drift canary; schema change requires a numbered
migration. This cuts both ways: adding envelope/tier/principal columns is
loud and reviewed, and any scheme requiring structural fields to become
opaque (encrypting type or resolution) fights the database itself.

## Known-stale risks in this document

Facts most likely to drift, in rough order: the claims-wiring status flag
(§3), the merge-tiebreak observation (§4), the exact config key set (§6),
and the retired-command list (§1). The invariants of §7 are the part
expected to survive; if one stops holding, that is a design-relevant event,
not a documentation error.
