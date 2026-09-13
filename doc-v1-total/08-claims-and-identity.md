# Claims and identity

lit tracks who is working on what without a stored assignment table. A **claim** — "this checkout is working that lane" — is derived at read time from history events the database already holds; there is no claims table, row, or file (`internal/claims/evidence.go:1-15`). The only persisted footprint is an opaque **attribution** stamp on each history event. Alongside claims, and separate from them, lit keeps a conventional **assignee** string on each issue. These are two distinct identity systems that never share a value:

| System | Value | Where it lives | Who reads it |
|---|---|---|---|
| Checkout identity (claims) | stream token + workspace id | `<private git dir>/lit-stream`, local only | claim derivation |
| Acting identity (assignee/actor) | `claude_<session>` or flag value | flags + `CLAUDE_CODE_SESSION_ID` env var | `Issue.Assignee`, `IssueEvent.Actor`, `CreatedBy` |

This document covers both systems, the derivation rules, and every command surface that consults claims.

## Checkout identity: the stream token

Every git checkout (worktree) that mutates a lit store carries a **stream token**: 8 bytes from `crypto/rand`, unpadded lowercase base32 — exactly 13 characters over the alphabet `[a-z2-7]` (`internal/workspace/stream.go:28-33`). The token lives in a file named `lit-stream` inside the checkout's *private* git dir (`--git-dir`), never the common dir — so every worktree of one repo shares one backlog but carries a distinct token, and `git worktree remove` deletes the token with the directory (`stream.go:13-22`). File content is the token plus a trailing newline, mode `0644` (`stream.go:175-187`).

Minting is write-once and race-safe: a temp file is written, synced, and hard-linked to the final name; `os.ErrExist` from the link means a racing caller already published and is treated as success — the file, never the freshly-minted candidate, decides identity (`stream.go:102-105, 150-227`). Reading a missing file yields the zero `StreamID` (a legitimate "never minted" state, not an error); a malformed file — wrong length or a character outside the alphabet — is an error that never self-heals, and the message tells the user to delete the file to mint a fresh identity, noting that recorded work keeps the old identity and any held lane is released (`stream.go:79-92, 255-282`).

**Which commands mint.** Store access mode and identity minting are paired in one table (`internal/app/app.go:57-60`): a write-mode open runs `EnsureStream` (mints if absent); a read-mode open runs `ReadStream` (never mints). So a read-only command in a never-mutated checkout finds no token and creates none; a read after a write sees exactly the minted token; two worktrees never share an identity (`internal/app/app_test.go:144-182`).

## Acting identity: assignee and actor strings

Every command that records an actor or assignee resolves it through one rule (`internal/cli/cli.go:1172-1177`):

1. If the `CLAUDE_CODE_SESSION_ID` environment variable is set (trimmed, non-empty), the identity is `"claude_" + sessionID` — **the env var always beats any flag**.
2. Otherwise the explicit flag value (`--assignee` on `start`; the hidden `--by` for other verbs), trimmed.
3. Otherwise `""`, which the store normalizes to the opaque `"unknown"` and displays as `(unassigned)` (`cli.go:1193-1215`).

`os.Getenv("USER")` was deliberately removed as a privacy violation; raw `$USER` never lands in `CreatedBy`/`Actor` (`cli.go:1196-1200`, `internal/cli/attribution_test.go:33-88`). `start` is the only lifecycle action that rewrites the assignee (`internal/model/lifecycle/action.go:44-49`); relation, label, and bulk verbs all resolve their `created_by`/actor through the same rule. One consequence: when the env var is set, two different checkouts flatten to one assignee, and a same-state `start` is a store-level no-op, so the claim does not transfer (`internal/cli/claims_takeover_e2e_test.go:19-26`).

## Attribution: the persisted primitive

`model.Attribution` is the pair `(stream, workspace)` — the checkout's stream token plus the store's workspace id (a UUID generated at init, `internal/workspace/workspace.go:492`). Its rules (detailed in `01-data-model.md`): complete-or-absent (either half empty collapses the pair to zero, silently, at every boundary including JSON decode), opaque by mandate (nothing user-, host-, or path-shaped, because the database syncs to shared remotes), written once at event creation and never backfilled (`internal/model/model.go:631-712`).

The pair reaches the database through one path: `app.Open` calls `Store.AttributeTo(streamToken)` unconditionally for both access modes; the store pairs the token with its own workspace id and stamps the pair at `recordEvent`, the single insertion point for issue history (`internal/app/app.go:105`, `internal/store/store.go:242-262`). An empty token leaves the store unattributed rather than half-attributed. `OpenSync`, `RebuildCandidate`, adopt, upgrade, and `OpenLocationForRead` do not stamp — they read, or replay dumps preserving the producer's attribution (`store.go:252-259`). Attribution survives a git-remote round trip: a second clone sees the producer's exact pair, never re-stamped (`internal/cli/claims_attribution_test.go:65-129`).

## The claim unit and the derived value

The unit a claim is held over is the **lane** (`model.LaneID`; see `01-data-model.md`): `(epic, lane-string)` for a child of an epic — the empty spelling included — or a "lane of one" keyed by the issue's own ID for a parentless issue or a child of a non-container parent (`internal/model/model.go:212-217`).

Derivation produces a **Standing** per lane, a sealed three-variant sum (`internal/claims/standing.go:17-64`):

| Standing | Meaning | Carries |
|---|---|---|
| `Unclaimed` | no holder | nothing |
| `Held` | a checkout holds the lane and its evidence is fresh | `Tenure` + `Contested []Attribution` |
| `Stale` | a checkout holds the lane but its evidence aged out | `Tenure` |

`Tenure` is `{By Attribution; Since time.Time; LastActivity time.Time}`: `Since` is the timestamp of the establishing event; `LastActivity` is the holder's most recent mutation of any kind in the lane — and freshness is measured against `LastActivity`, not `Since` (`standing.go:28-49`). `Standings` is a map keyed by lane whose read is total: a missing key returns `Unclaimed`, never a nil (`standing.go:71-78`).

## Evidence assembly

`claims.Evidence` groups issues and events by lane (`internal/claims/evidence.go:28-31`). `NewEvidence(issues, parents, events)`:

- Maps each issue to its lane via `LaneOf` (an issue absent from the parents map is parentless) and records lane membership (`evidence.go:52-62`).
- **Refuses a partial read**: an event whose issue is not among the supplied issues is an error — "claim derivation needs every issue the events touch, closed ones included", because a `done` on a now-closed ticket can be the sole establishing act (`evidence.go:39-46, 66`).
- Sorts each lane's events by `(created_at, id)` — total and stable, so derivation is independent of input order (`evidence.go:75-79`).

`LaneProgress(lane)` counts a lane's members: `Total` = all members, `Done` = members whose state is closed, `Active` = the in-progress member (last such member wins if several). An unseen lane returns the zero value (`evidence.go:99-120`).

**Establishing acts.** Exactly two of the eight lifecycle verbs establish a claim: `start` (taking work) and `done` (the neutral success close — a checkout that just completed a ticket mid-lane still holds the lane). `close` (with any outcome), `reopen`, and the four retention verbs never establish; neither does a plain field update (empty action) or an unrecognized verb (`internal/claims/establish.go:11-56`). The classification is a map covering every verb so a ninth action added to the vocabulary fails a coverage test rather than silently defaulting (`internal/claims/establish_internal_test.go:14-23`).

## Freshness

`Freshness{Now, Window}` travels as data — the derivation reads no clock. The single comparison is `Covers(t) = !t.Before(Now-Window)`, i.e. `t >= Now-Window`: **evidence exactly on the boundary is covered** (`internal/claims/derive.go:20-32`).

The window comes from config key `claims.freshness_window`, default `"6h"` (`internal/config/config.go:228`). It is parsed as a duration *string*, deliberately not struct-tag decoded: a bare `72` would weak-decode to 72 nanoseconds, positive and passing validation, expiring every claim instantly. A non-duration value or a non-positive duration is a config error with a message naming the required form (`config.go:56-95, 262-266`). At runtime `Now` is `time.Now()` taken in `gatherClaimContext` (`internal/cli/claims_context.go:101`).

## Local liveness

A machine can positively prove one thing remotes cannot: that a checkout of *its own workspace* no longer exists. `claims.LocalCheckouts` carries this proof: a workspace id plus the set of live stream tokens (`internal/claims/local.go:27-41`). An event is **void** iff its attribution is present *and* its workspace equals this machine's workspace *and* its stream token is not in the live set (`local.go:58-64`). An unattributed event can never be void — which also keeps the zero value (empty workspace, empty set) inert. The zero value means "this machine enumerated nothing and proves nothing"; callers that cannot enumerate must pass it, never a guess (`local.go:19-34`).

The asymmetry (`local.go:11-17`): worktree deletion is a local fact — a claim from a deleted checkout dies here at once; everywhere else (other machines, other clones) it waits out the freshness window. A different clone on the same machine carries a different workspace id and is never pruned. This is proven end to end: after `git worktree remove --force`, the primary's very next derivation reports `Unclaimed` with no waiting and no cleanup step, while a second clone deriving the same evidence still reports `Held` (`internal/app/claims_test.go:101-173`).

**Enumeration** runs `git worktree list --porcelain -z` (requires git ≥ 2.36, named in every failure message) and parses NUL-separated fields: `worktree <path>` opens a record; recognized attributes are `branch` (with `refs/heads/` trimmed; empty for detached HEAD), `prunable`, `bare`, and `locked`; unknown keys are ignored (`internal/workspace/checkouts.go:191-229`, the attribute switch at `:211-218`). `locked` is read but is deliberately not a vote on liveness — git already withholds `prunable` from a locked record, so dropping the record on `locked` would be a second vote on the same fact — and it leaves the enumeration as `Checkout.Locked` (`:116`), becoming the `claims.Locked` presence the routing relation and the takeover gate both read (`internal/app/claims.go:65`, `internal/claims/local.go:107-108`). Records that are `prunable` or `bare` are dropped as uninhabited — git's judgment, which correctly handles both an `rm -rf`'d worktree (prunable despite its leftover git dir) and a locked worktree on removable media (not prunable despite a missing directory) (`checkouts.go:41-50, 132`). Zero records is an error (git always lists the current worktree; reporting zero would void every claim), and any per-checkout read failure aborts the whole enumeration rather than silently asserting a checkout is deleted (`checkouts.go:56-60, 101-108, 207-213`). Checkouts without a minted token are then dropped: a never-mutated checkout carries no token, holds no claim, and voids nothing (`internal/app/claims.go:41-62`).

## Derivation: the four-legged predicate

`Derive(evidence, fresh, local)` computes a standing per lane; it writes nothing (`internal/claims/derive.go:34-43`). Per lane, `standingOf` runs four legs in dependency order **1, 4, 2, 3** (`derive.go:49-55`):

**Leg 1 — the lane is unfinished.** If no member is in play (`InPlay` = not archived/deleted and not closed; see `01-data-model.md`), the lane is `Unclaimed` — a claim on finished work does not exist (`derive.go:62-64`). An all-closed lane and a lane whose sole ticket is archived both derive `Unclaimed`.

**Leg 4 — the holder is live as far as this machine can tell.** Events void under `LocalCheckouts` are filtered out *before* asking who established latest — over a clone of the event slice, so one derivation cannot strip events from shared evidence and change a second derivation's answer (`derive.go:73-81`). The ordering matters: filtering first means a voided newer `start` falls through to whoever else has standing (the lane reverts to an older establisher's claim) rather than reading unclaimed (`derive.go:50-54`).

**Leg 2 — the holder produced the latest establishing event.** The latest establishing event among the admissible events names the holder. If there is none, or the latest one is **unattributed**, the lane is `Unclaimed` — the derivation stops at an unattributed latest establisher rather than scanning back to an older attributed one, because an unattributed `start` says somebody took the lane and the record does not say who; an older attributed event is positively known to be superseded (`derive.go:87-103, 119-126`). (Contrast: a *void* event is disproven rather than unknown, hence falls through.) A repository whose whole history predates attribution derives `Unclaimed` everywhere — exactly the pre-claims behavior (`derive.go:96-98`). Tenure is assembled from the establisher's timestamp and the holder's last activity of any kind in the lane (`derive.go:105, 132-142`).

**Leg 3 — the claim is fresh.** If the freshness window does not cover `LastActivity`, the standing is `Stale`; otherwise `Held` with contest annotations (`derive.go:112-115`). Because freshness reads `LastActivity`, ordinary commentary or a bare field edit carries a claim through a long stretch: a `start` 80 hours ago plus an edit 30 minutes ago is `Held` under a 24-hour window.

**Contest.** Contestants are checkouts *with an establishing act of their own* in the lane (a drive-by comment or grooming edit never contests), excluding the holder, unattributed pairs, and any candidate whose own last activity aged out of the window. Sorted most-recently-active first, tie-broken by stream string; empty (non-nil) when nobody contests (`derive.go:147-170`). Contest is an annotation, not a state: routing is unaffected and the holder remains the holder (`internal/claims/standing.go:41-46`). Events from foreign workspaces are never pruned by local liveness, so a foreign holder stays `Held` even when this machine enumerates zero live streams.

Summary of the legs (all under a 24 h window; from the pinned test grid):

| Dropped leg | Fixture | Result |
|---|---|---|
| none | `start` by A at −2 h, both streams live | `Held{A}` |
| 1 | all tickets closed, or sole open ticket archived | `Unclaimed` |
| 2 | only `reopen`/`archive`/`close`/bare edits | `Unclaimed` |
| 2 | A's `start` at −3 h, unattributed `start` at −1 h | `Unclaimed` |
| 3 | `start` −72 h, edit −48 h | `Stale{A}` |
| 4 | `start` by A, A's checkout no longer live | `Unclaimed` |

## Gathering the claim context in the CLI

`gatherClaimContext` assembles everything a command needs (`internal/cli/claims_context.go:43-108`): load config; list issues with **both** `IncludeArchived` and `IncludeDeleted` set (an establishing event can sit on a deleted or archived issue, and evidence assembly refuses partial reads); fetch parents; list all events; build evidence; enumerate live checkouts — on enumeration failure it prints `warning: could not enumerate local checkouts (…) — claim liveness check and local addresses skipped, freshness alone governs` and proceeds with the zero `LocalCheckouts`; derive standings with `Now = time.Now()`; and compute `self` as `NewAttribution(stream, workspaceID)` — a never-minted stream collapses to the zero attribution, which reads as "no live claims" with no special branch (`claims_context.go:103-106`). It also builds an `addresses` map from attribution to live local checkout (path + branch) that never reaches the shared database and lives only for the process (`claims_context.go:22-38, 128-136`).

Callers: `lit next`, the backlog/workable runner, `lit start`'s authorization, and the sync-reconcile contest report.

## Gates: where claims change behavior

### `lit start` — the takeover gate (the only write gate)

`start` is the only transition with an authorization hook; it runs after the action is built and before the store apply, and can abort the transition (`internal/cli/cli.go:1237-1283, 1383-1388`). The issue's lane standing and the caller's own attribution classify the requirement (`internal/cli/claims_takeover.go:38-53`):

| Standing | Condition | Requirement |
|---|---|---|
| `Held` | held by self | none |
| `Held` | held by another | fresh-confirm |
| `Stale` | held by self | none |
| `Stale` | held by another, holder `claims.Locked` | fresh-confirm |
| `Stale` | held by another, otherwise | stale-informed |
| `Unclaimed` | — | none |

- **None**: proceed; the happy path costs one extra evidence gather and nothing else.
- **Stale-informed**: proceeds unprompted, printing the claim line plus ` — check for unmerged branches or PRs on this lane before building on it`. Checking is left to the taking agent; lit stays ignorant of git branches and the forge (`printStaleProvenance`, `claims_takeover.go:177-184`). An expired claim reaches this arm only when its holder is not `claims.Locked`; a locked worktree takes fresh-confirm instead, per the row above.
- **Fresh-confirm**: on a non-interactive stdout, refuses unless `--take` was passed (`… — this lane is claimed and active; pass --take to confirm the takeover`); with `--take`, prints `… — taking over (--take)` and proceeds. On an interactive terminal, prompts `take over this lane? [y/N]` reading stdin; any answer whose trimmed lowercase form starts with `y` proceeds, anything else fails with `takeover declined` (`claims_takeover.go:129-152`). The `--take` flag's help: "Confirm taking over a lane another checkout claims right now (required for non-interactive callers; an interactive terminal is prompted instead)" (`cli.go:1278`).

Proven over two real clones and a git remote: the second clone's plain `start` fails naming `--take` and `claimed`; with `--take` it succeeds printing "taking over"; a subsequent `start` on the now-transferred lane prompts nothing (`internal/cli/claims_takeover_e2e_test.go:18-80`).

### `lit next` — claim-aware routing (a read gate)

`next` routes over rows in composite-rank order to one of six sealed outcomes (`internal/cli/next_route.go:26-184`). It is registered `app.AccessRead` (`internal/cli/register.go:485`) and writes nothing: it claims no lane and starts no ticket, so every line it prints either reports a state that already holds or is advice about a command the reader has yet to run (`internal/cli/next.go:79-93`).

| Outcome | Carries | Meaning |
|---|---|---|
| `ServedFromClaim` | `Row` | a startable ticket in a lane this checkout already holds |
| `ResumedOwnWork` | `Row` | a ticket already in flight in a lane this checkout holds, handed back to its holder |
| `ServedFromEpicLane` | `Row`, `Lane model.LaneID` | a pick from a different lane of an epic this checkout already holds a lane in |
| `ServedFromNewLane` | `Row`, `Lane model.LaneID` | a pick in a lane this checkout does not hold — from the global pool or from an on-path dependency |
| `Exhausted` | `Epics`, `Blocked` | the checkout's own epic(s) have open work, none of it reachable; returned as an error |
| `NoWork` | `Unreachable` | the global pool produced nothing; returned as an error |

`ServedFromEpicLane` carries exactly `Row` and `Lane`; there is no `Epic` field, because the epic is `Lane.Epic()` and storing it beside the lane was two clocks for one fact. `ServedFromNewLane` likewise carries a `model.LaneID`, not a pre-rendered string.

**Precedence** (`routeNext`, `next_route.go:307-390`). `ownScope` (`next_route.go:265`) derives the checkout's own lanes and epics from the **standings**, not from the gathered rows: rows are already narrowed by `--type/--labels/--assignee`, and deriving ownership from them let a display filter empty the set and drop the whole self-aware branch. A checkout holding at least one lane tries, in order: **step 1**, its own lanes, accepting `{serveWork, resumeWork}` and yielding `ResumedOwnWork` or `ServedFromClaim`; **step 1b**, an on-path dependency — a row outside our lanes that gates one of them (`onPathDependency`, `next_route.go:469`) — yielding `ServedFromNewLane`, since starting it would establish a claim on a lane we do not hold; **step 2**, the rest of our epic in lanes we do not already hold, accepting `{serveWork, takeoverWork}` and yielding `ServedFromEpicLane`; **step 3**, `Exhausted`, which is terminal and never falls through to the global pool. A checkout holding no lanes starts instead at **step 4**, the focus-scoped global pool (`--all` routes over the whole queue), which accepts `{serveWork, takeoverWork}` and yields `ServedFromNewLane`, otherwise `NoWork` — whose `Unreachable` carries both the rows the walk passed over and the rows the scope withheld, because "nothing is startable" and "nothing on your focus path is startable" are different answers.

**Admission** is `capacityFor` (`next_route.go:229`), reading the row's readiness classification, its lifecycle state, and the lane's relation to this checkout (`relationOf`, `internal/cli/claims_takeover.go:68-101`), and returning one of four capacities. In a lane this checkout holds — `laneOurs`, fresh or stale — an in-progress row is `resumeWork`, a ready row is `serveWork`, anything else `routeAround`. Elsewhere a row is takeable when it is in progress and classified orphaned, or not in progress and ready; a takeable row is `takeoverWork` if it is in progress or its lane is `laneStaleForeign`, and `serveWork` otherwise. `laneHeldForeign` is always `routeAround`, so a stale foreign lane is a legitimate bare-`next` target and the lane held fresh by another checkout is what routing goes around — with one qualification: a `Stale` standing whose `Holder` is `claims.Locked` reports `laneHeldForeign` too, so a locked worktree is routed around and gated exactly as a fresh hold is, however far its clock has expired. `git worktree lock` is the holder deliberately saying do-not-disturb, while a worktree merely existing is what a deleted session leaves behind just as readily — so presence without a lock does not sustain the claim, and the age-out exists precisely for that case. `accept` is a set and never a preference order: composite rank is the only tiebreak routing applies, and ranking capacities against each other would reintroduce the symptom the set fixed — the backlog's #1 row, an orphan, passed over for a lower-ranked leaf that needed no takeover.

Servability does not require `status == open`. Step 1 accepts `resumeWork`, so an in-progress row in the checkout's own lane is handed back to resume; while routing gated servability on `model.StateOpen`, an `in_progress` row was servable to nobody, which hid every orphan and the very ticket the checkout was working at that moment.

`Exhausted` and `NoWork` implement `error` and travel outward as themselves rather than being rendered into a generic error, which is what keeps the exit-code and reason sinks reading the routing verdict instead of a copy that could drift. Both exit **6** (`ExitNoWork`, `internal/cli/exit.go:30`), with reasons `scope_exhausted` and `no_ready_work` (`internal/cli/error_output.go:113,117`). Six rather than `ExitGeneric`, because a caller looping `lit next` has to tell "stop, there is nothing for you" from "lit is broken" without parsing the English; not `ExitOK`, because for `lit next` 0 means a ticket is on stdout, and exiting 0 with no row would hand the caller a success-shaped void.

Both diagnostics are written in `reachKind`, which says what one row is to this checkout right now: `reachTakeable`, `reachHeldFresh`, `reachNotReady`, `reachOutOfView`, and `reachOffFocusPath`, the last used only by the pool diagnostic. A bool here read "takeable or not", so a row outside the run's filtered view, or one not startable itself, rendered as the one reason the message named: claimed by another checkout. Exhaustion asks `reachKind` of the dependencies gating our scope; an empty global pool asks it of every row the walk went past. Each clause names at most twelve ids and says how many it left out (`maxNamedPerKind`, `next_route.go:540`); the per-kind wordings and both error formats are in inventory-claims.md §9.2.

After routing, `next` prints the advice line above any pick that would establish a claim — naming what running `lit start` would lock rather than what `next` did, since reporting an act is the one thing a read-only command must not do — then the ticket summary with its claim line, and dispatches the pulled-ticket workflow occasion (`next.go:94-135`, `startAdvice` at `next.go:183`).

### `lit sync reconcile` — the contest report

After a reconcile whose outcome actually merged histories (linearized or combined — not prose-pending, unrelated-histories, or not-diverged), lit reports lanes where evidence from more than one checkout just met (`internal/cli/sync_reconcile_cmd.go:519, 543`). The report lists every `Held` lane with a non-empty contested set, sorted by lane string, under the header `contested: evidence from more than one checkout just met for these lanes —`, each with its claim line (`internal/cli/claims_contest_report.go:32-74`). The gather runs without a `Stream`, so `self` is zero for this call. Nothing is printed when no lane is contested.

### Render-only surfaces

`lit backlog` prints the claim line (indented, between the `in_progress:` and `unblocks:` lines), and `lit next`'s summary block prints it between `depends on` and `unblocks` (`internal/cli/backlog.go:92-96`, `internal/cli/ready_state.go:601-614`). No other command consults standings; every other consumer of the claim context uses it only for rendering.

## Rendering the claim line

`formatClaimLine` renders `Held` and `Stale` only — an `Unclaimed` lane renders no line at all (`internal/cli/claims_render.go:23-44`). The line joins with ` · `: the holder badge, the coarse age of `LastActivity`, and lane progress when the lane has members.

- **Holder badge** (`claims_render.go:52-69`): if the holder resolves to a live local worktree, `claimed here[ (stale)]: <path> (<branch>)` — branch shown as `detached HEAD` when empty; a stale claim from a still-live local worktree still resolves to its address, stale controlling only the label. Otherwise `claimed: stream <short> (elsewhere)` or `(stale)`.
- **Contest suffix** on a contested `Held`: ` · contested by <short-streams>`.
- **Lane progress** (`claims_render.go:76-84`): `""` for an empty lane; `<active-id> in progress, <done>/<total> done` when a member is in progress; else `<done>/<total> done`.
- **Short streams**: tokens truncated to the first 8 characters for display — a nicety, not a privacy measure, since the full token is already opaque (`claims_render.go:86-107`).
- **Coarse durations** (`internal/cli/output.go:451-462`): ≥ 48 h → `N days`; ≥ 2 h → `N hours`; ≥ 2 m → `N minutes`; else `under a minute`.

The two tiers are deliberate: the dossier (holder, freshness, progress) comes entirely from shared synced data and renders identically on any clone; the local path/branch renders only on the machine that enumerated the holder's worktree (`claims_render.go:16-22`).

## The `internal/app` service layer

`internal/app` is the seam between the CLI and the store: two files, and this complete surface (`internal/app/app.go`, `internal/app/claims.go`).

- **`App`** — `{Workspace workspace.Info; Store storage.Store; Stream workspace.StreamID}`. `Stream` is always present under write access (minted on the checkout's first mutating command); present under read access only if an earlier mutation minted it — absence is the honest report that the checkout holds no claim (`app.go:13-24`).
- **`Open(ctx, cwd, mode)`** — in order: validate the mode by table lookup (unknown, including `""`, → `invalid access mode`); resolve the workspace from cwd; open the store engine (write mode bootstraps a missing database; read mode fails with "not initialized"); resolve the stream identity *after* the store opens, so a command that cannot reach its store mints nothing; on identity failure close the store (which also releases the workspace lock) and join both errors; call `AttributeTo` unconditionally (`app.go:65-107`). A malformed token file fails the open with a "malformed" diagnosis and releases the store — proven by a repaired second open succeeding.
- **`OpenLocationForRead(ctx, loc)`** — opens a store at an already-derived location, bypassing cwd git resolution; the cross-project primitive for aggregating over many stores. Reads the foreign store's `workspace_id` from its own `config.json` (a pure read), always opens read-only (so the foreign store gets the shared lock, never a second read-write engine the embedded driver would reject), mints no identity, and stamps nothing (`app.go:109-131`).
- **`Close()`** — `Store.Close()` (`app.go:133`).
- **`LocalCheckouts()`** — enumerates live checkouts and scopes them to this workspace id; on error returns the zero value *and* the error — it reports what it proved or that it proved nothing (`claims.go:26-37`).

The package emits no events and publishes no observer surface.

## Privacy invariants

- Both halves of an attribution are opaque by mandate; nothing user-, host-, or path-shaped travels there, because the database syncs to shared remotes. Resolving a token to a physical checkout happens only on the machine that owns it (`internal/model/model.go:624-627`).
- The stream token is deliberately meaningless — no directory, hostname, or username material (`internal/workspace/stream.go:40-49`).
- The `--by` fallback is `""` (normalized to `"unknown"`), the old `$USER` default having been removed as an invariant violation (`internal/cli/cli.go:1196-1200`).
- Checkout paths and branches stay on the local machine; the address map lives only for the process (`internal/workspace/checkouts.go:21-26`, `internal/cli/claims_context.go:30-32`).
- A different clone on the same machine has a different workspace id, so this machine's enumeration never speaks to its claims (`internal/app/claims.go:21-24`).
