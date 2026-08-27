# lit: what it is and how it works

lit ("links issue tracker") is an issue tracker that lives inside a git repository and is operated primarily by coding agents. There is no server, no accounts, and no web UI: `lit` is a single Go binary, the backlog is an embedded [Dolt](https://github.com/dolthub/dolt) SQL database stored under the repository's git common directory, and every command runs in a checkout of the repo it tracks. Where Jira separates the tracker from the code, lit collapses them: the backlog travels with the repository, syncs through the repository's ordinary git remote, and versions itself the way the code does — every mutation is a database commit.

The intended operator split is explicit in the command help: humans run the bootstrap and maintenance commands (`init`, `doctor`, `sync`, backups); agents run the work loop (`next`, `start`, `update`, `done`). Most agent-facing surfaces assume the caller is an LLM session — error remediations embed `<agent-instructions>` blocks, guidance text is injected into command output at lifecycle moments, and the acting identity defaults to `claude_<session-id>` when a Claude Code session variable is present.

## The shape of the data

The unit of work is the **issue** — id, title, description, an optional reusable agent `prompt`, a two-value priority, one of five types (`task`, `feature`, `bug`, `chore`, `epic`), a topic slug baked into the id, labels, an assignee, and an ordering rank. An **epic** is not a separate record type: it is an issue whose type is `epic`, whose open/closed state is computed from its children at read time and never stored, and which rejects direct status transitions.

Around the issue sit typed **relations** (`parent-child`, `blocks`, `related-to`), flat **comments**, **labels**, and an append-only **event history** — one event per mutation, carrying the actor, an optional reason, and the field-by-field before/after values. A leaf issue's status is a three-state machine (`open` → `in_progress` → `closed`) driven by eight named verbs; closes either record no resolution (`done`, the neutral success) or a mandatory one (`close`, with `duplicate`/`superseded`/`obsolete`/`wontfix`, the first two redirecting to a canonical ticket). A second, orthogonal axis — retention — moves issues between `live`, `archived`, and `deleted` without touching status. Chapter 01 specifies all of it.

Ordering is a global **rank**: a base-62 string whose lexicographic order is the backlog order, maintained by fractional indexing so any insertion lands between two neighbors without renumbering. Ranks are frame-local — children rank against siblings inside their epic — and `lit rank` on a child moves the whole epic. Within an epic, **lanes** partition children into parallel rank-ordered sequences: same lane means strictly sequenced, different lanes proceed in parallel.

## The work loop

A repository joins lit with `lit init`, which — before creating anything — checks whether the git remote already carries a backlog and clones it if so; otherwise it creates an empty store. Init also installs a managed `pre-push` git hook and managed sections in `AGENTS.md`/`CLAUDE.md` that tell agents to start with `lit quickstart`, the built-in guidance router.

From there the agent loop is:

1. **`lit backlog`** — the ranked queue, annotated per row with why it is or isn't workable.
2. **`lit next`** — routes to one ticket. "Workable" is a computed predicate: open, a leaf, no unmet required fields, no open dependencies, no earlier open sibling in the same lane, not labeled `needs-design`. Ordering is composite rank, then priority, then a `focus` label's prerequisite closure.
3. **`lit start <id>`** — takes the work. If another checkout actively holds the ticket's lane, start refuses without an explicit `--take` (or an interactive confirmation).
4. Work happens in the repo; `lit update`, `lit comment`, `lit dep`, `lit label` record progress.
5. **`lit done`** (success) or **`lit close --resolution …`** (any other ending); `lit followup` files a successor parented to what just closed.

At each of these moments, matching **workflow** definitions — markdown files with label/state/event matchers, overridable per project — inject their body text into the command's output. They are guidance, not automation: nothing executes.

## Coordination without a server

Two clones of a repo each have a full copy of the backlog, so lit's multi-writer story is a merge story, modeled on git:

- **Every mutation is one Dolt commit.** Sync is fetch / fast-forward / push against a Dolt remote derived from the repo's git remote (the data rides in `refs/dolt/*` alongside the git refs — no extra infrastructure).
- **A background sync engine** keeps this invisible in the common case: after every successful mutating command, lit spawns a short-lived detached mirror process that pushes, runs a debounced inline receive, and compacts the store on a backstop cadence. Read commands print staleness banners when pushes are failing or fetches are old; a configurable owner-notify hook can page a human.
- **Divergence is merged field-by-field.** When both sides committed since their common ancestor, `reconcile` performs a deterministic three-way merge per issue per field (status joins toward the more-terminal state, priority takes the higher, labels merge per name, timestamps never decide winners) and replays local history onto the remote head, preserving commit provenance. Free-text fields (`title`, `description`, `prompt`) are never auto-picked: a prose conflict is held, both sides intact, until an agent or human supplies resolutions.
- **Destructive choices need the owner.** Two backlogs with no common ancestor can be combined (union, no approval), or one side can be taken wholesale — but a take requires an approval token bound to the exact fork, designed so an agent cannot self-approve discarding work.

**Claims** are how checkouts avoid colliding on the same work, and they are derived, never stored. Every checkout carries an opaque random stream token; every event it writes is stamped with (token, workspace-id). At read time, lit derives who holds each **lane** — the claim unit — from the latest establishing event (`start` or `done`), a freshness window (default 24h of any activity), and local worktree liveness (a deleted worktree's claims die immediately on its own machine). `lit next` routes around held lanes and continues the caller's own epics first; `lit start` gates takeovers. Nothing identifying — usernames, hostnames, paths — ever enters the shared database.

## Safety and recovery

The store defends itself in layers: kernel file locks with no stale-lock heuristics (process death is the only release); a byte-frozen schema baseline plus versioned migrations with pre-migration snapshots, checkpoints, and a quarantine table that blocks a workspace after a failed migration rather than corrupting it; `lit doctor` (check) and `--fix` (repair); JSON export backups with a restore that refuses to clobber unsynced work; filesystem-level snapshots; and, for a store the engine refuses to open at all, the `lifeboat` pipeline — raw-dump every table, map the dump's shape into an export, rebuild a disposable candidate, verify it conserves the source, and atomically promote it while preserving the previous contents. `upgrade`/`downgrade` move the binary and schema in lockstep, each direction ordered so a failure leaves a recoverable state.

## Chapter map

| Chapter | Contents |
|---|---|
| [01 — Data model](01-data-model.md) | Issues, epics, lifecycle, retention, resolutions, lanes, relations, events, attribution, IDs, ranks, the export format |
| [02 — Storage contract](02-storage-contract.md) | The engine-agnostic store interface, filters and sorts, capabilities, the in-memory reference engine, what conformance guarantees |
| [03 — Dolt store: schema and records](03-store-schema.md) | The SQL schema, commit and locking semantics, create/read/update/event/label/relation/rank operations, the shapemap |
| [04 — Dolt store: operations](04-store-operations.md) | Export/import, delta application, Doctor, disaster recovery (dump → rebuild → verify → promote), checkpoints, the lock inventory, the vendored driver |
| [05 — Sync, merge, compaction, migrations](05-sync-merge-compaction.md) | The sync algorithm, field-level merge rules, prose conflicts, unrelated-history takes, Dolt GC, the migration runner, recovery snapshots |
| [06 — CLI: issue commands](06-issue-commands.md) | Every issue-facing command, the query grammar, the workability predicate, exit codes, output shapes |
| [07 — CLI: operations and the sync engine](07-ops-commands-and-sync-engine.md) | `init`, the `sync` family, the background mirror, doctor, backups, snapshots, lifeboat, quickstart, managed files |
| [08 — Claims and identity](08-claims-and-identity.md) | Stream tokens, acting identity, attribution, the derivation predicate, takeover and routing gates |
| [09 — Workflows and events](09-workflows.md) | The guidance-injection system: definition format, event catalog, matching, layering |
| [10 — Platform](10-platform.md) | Startup and signals, env vars, configuration, workspace discovery, releases, CI and build tooling |

The `inventories/` directory holds the raw per-subsystem behavioral inventories the chapters were distilled from, with verbatim error strings and test citations.
