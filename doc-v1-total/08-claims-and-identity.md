# Claims and identity

lit tracks who is working on what without a stored assignment table. A **claim** — "this checkout is working that lane" — is derived at read time from history events the database already holds; there is no claims table, row, or file (`internal/claims/evidence.go`). The only persisted footprint is an opaque **attribution** stamp on each history event. Alongside claims, and separate from them, lit keeps a conventional **assignee** string on each issue. These are two distinct identity systems that never share a value:

| System | Value | Where it lives | Who reads it |
|---|---|---|---|
| Checkout identity (claims) | stream token + workspace id | `<private git dir>/lit-stream`, local only | claim derivation |
| Acting identity (assignee/actor) | `claude_<session>` or flag value | flags + `CLAUDE_CODE_SESSION_ID` env var | `Issue.Assignee`, `IssueEvent.Actor`, `CreatedBy` |

This document covers both systems, the derivation rules, and every command surface that consults claims.

## Checkout identity: the stream token

Every git checkout (worktree) that mutates a lit store carries a **stream token**: 8 bytes from `crypto/rand`, unpadded lowercase base32 — exactly 13 characters over the alphabet `[a-z2-7]` (`internal/workspace/stream.go`). The token lives in a file named `lit-stream` inside the checkout's *private* git dir (`--git-dir`), never the common dir — so every worktree of one repo shares one backlog but carries a distinct token, and `git worktree remove` deletes the token with the directory (`stream.go`). File content is the token plus a trailing newline, mode `0644` (`stream.go`).

Minting is write-once and race-safe: a temp file is written, synced, and hard-linked to the final name; `os.ErrExist` from the link means a racing caller already published and is treated as success — the file, never the freshly-minted candidate, decides identity (`stream.go`). Reading a missing file yields the zero `StreamID` (a legitimate "never minted" state, not an error); a malformed file — wrong length or a character outside the alphabet — is an error that never self-heals, and the message tells the user to delete the file to mint a fresh identity, noting that recorded work keeps the old identity and any held lane is released (`stream.go`).

**Which commands mint.** Store access mode and identity minting are paired in one table (`internal/app/app.go`): a write-mode open runs `EnsureStream` (mints if absent); a read-mode open runs `ReadStream` (never mints). So a read-only command in a never-mutated checkout finds no token and creates none; a read after a write sees exactly the minted token; two worktrees never share an identity (`internal/app/app_test.go`).

## Acting identity: assignee and actor strings

Every command that records an actor or assignee resolves it through one rule (`internal/cli/cli.go`):

1. If the `CLAUDE_CODE_SESSION_ID` environment variable is set (trimmed, non-empty), the identity is `"claude_" + sessionID` — **the env var always beats any flag**.
2. Otherwise the explicit flag value (`--assignee` on `start`; the hidden `--by` for other verbs), trimmed.
3. Otherwise `""`, which the store normalizes to the opaque `"unknown"` and displays as `(unassigned)` (`cli.go`).

`os.Getenv("USER")` was deliberately removed as a privacy violation; raw `$USER` never lands in `CreatedBy`/`Actor` (`cli.go`, `internal/cli/attribution_test.go`). `start` is the only lifecycle action that rewrites the assignee (`internal/model/lifecycle/action.go`); relation, label, and bulk verbs all resolve their `created_by`/actor through the same rule. One consequence: when the env var is set, two different checkouts flatten to one assignee, and a same-state `start` is a store-level no-op, so the claim does not transfer (`internal/cli/claims_takeover_e2e_test.go`).

## Attribution: the persisted primitive

`model.Attribution` is the pair `(stream, workspace)` — the checkout's stream token plus the store's workspace id (a UUID generated at init, `internal/workspace/workspace.go`). Its rules (detailed in `01-data-model.md`): complete-or-absent (either half empty collapses the pair to zero, silently, at every boundary including JSON decode), opaque by mandate (nothing user-, host-, or path-shaped, because the database syncs to shared remotes), written once at event creation and never backfilled (`internal/model/model.go`).

The pair reaches the database through one path: `app.Open` calls `Store.AttributeTo(streamToken)` unconditionally for both access modes; the store pairs the token with its own workspace id and stamps the pair at `recordEvent`, the single insertion point for issue history (`internal/app/app.go`, `internal/store/store.go`). An empty token leaves the store unattributed rather than half-attributed. `OpenSync`, `RebuildCandidate`, adopt, upgrade, and `OpenLocationForRead` do not stamp — they read, or replay dumps preserving the producer's attribution (`store.go`). Attribution survives a git-remote round trip: a second clone sees the producer's exact pair, never re-stamped (`internal/cli/claims_attribution_test.go`).

## The claim unit and the derived value

The unit a claim is held over is the **lane** (`model.LaneID`; see `01-data-model.md`): `(epic, lane-string)` for a child of an epic — the empty spelling included — or a "lane of one" keyed by the issue's own ID for a parentless issue or a child of a non-container parent (`internal/model/model.go`).

Derivation produces a **Standing** per lane, a sealed two-variant sum (`internal/claims/standing.go`):

| Standing | Meaning | Carries |
|---|---|---|
| `Unclaimed` | no holder — never started, finished, or the claim's evidence has aged out | nothing |
| `Held` | a checkout holds the lane right now | `Tenure` + `Contested []Attribution` |

There is no standing for a claim whose evidence has aged out. The lane derives `Unclaimed`, the same value a lane nobody ever started derives, and the type has nowhere to record that a claim existed (`standing.go`). A third variant, `Stale`, once carried the lapsed holder's tenure and worktree state and was read by every consumer as a grade of hold; it was removed under links-claims-y6yz.

`Tenure` is `{By Attribution; Since time.Time; LastActivity time.Time}`: `Since` is the timestamp of the establishing event; `LastActivity` is the holder's most recent mutation of any kind in the lane — and freshness is measured against `LastActivity`, not `Since` (`standing.go`). `Standings` is a map keyed by lane whose read is total: a missing key returns `Unclaimed`, never a nil (`standing.go`).

## Evidence assembly

`claims.Evidence` groups issues and events by lane (`internal/claims/evidence.go`). `NewEvidence(issues, parents, events)`:

- Maps each issue to its lane via `LaneOf` (an issue absent from the parents map is parentless) and records lane membership (`evidence.go`).
- **Refuses a partial read**: an event whose issue is not among the supplied issues is an error — "claim derivation needs every issue the events touch, closed ones included", because a `done` on a now-closed ticket can be the sole establishing act (`evidence.go`).
- Sorts each lane's events by `(created_at, id)` — total and stable, so derivation is independent of input order (`evidence.go`).

`LaneProgress(lane)` counts a lane's members: `Total` = all members, `Done` = members whose state is closed, `Active` = the in-progress member (last such member wins if several). An unseen lane returns the zero value (`evidence.go`).

**Establishing acts.** Exactly two of the eight lifecycle verbs establish a claim: `start` (taking work) and `done` (the neutral success close — a checkout that just completed a ticket mid-lane still holds the lane). `close` (with any outcome), `reopen`, and the four retention verbs never establish; neither does a plain field update (empty action) or an unrecognized verb (`internal/claims/establish.go`). The classification is a map covering every verb so a ninth action added to the vocabulary fails a coverage test rather than silently defaulting (`internal/claims/establish_internal_test.go`).

## Freshness

`Freshness{Now, Window}` travels as data — the derivation reads no clock. The single comparison is `Covers(t) = !t.Before(Now-Window)`, i.e. `t >= Now-Window`: **evidence exactly on the boundary is covered** (`internal/claims/derive.go`).

The window comes from config key `claims.freshness_window`, default `"6h"` (`internal/config/config.go`). It is parsed as a duration *string*, deliberately not struct-tag decoded: a bare `72` would weak-decode to 72 nanoseconds, positive and passing validation, expiring every claim instantly. A non-duration value or a non-positive duration is a config error with a message naming the required form (`config.go`). At runtime `Now` is `time.Now()` taken in `gatherClaimContext` (`internal/cli/claims_context.go`).

## Local liveness

A machine can positively prove one thing remotes cannot: that a checkout of *its own workspace* no longer exists. `claims.LocalCheckouts` carries this proof: a workspace id plus the set of live stream tokens (`internal/claims/local.go`). An event is **void** iff its attribution is present *and* its workspace equals this machine's workspace *and* its stream token is not in the live set (`local.go`). An unattributed event can never be void — which also keeps the zero value (empty workspace, empty set) inert. The zero value means "this machine enumerated nothing and proves nothing"; callers that cannot enumerate must pass it, never a guess (`local.go`).

The asymmetry (`local.go`): worktree deletion is a local fact — a claim from a deleted checkout dies here at once; everywhere else (other machines, other clones) it waits out the freshness window. A different clone on the same machine carries a different workspace id and is never pruned. This is proven end to end: after `git worktree remove --force`, the primary's very next derivation reports `Unclaimed` with no waiting and no cleanup step, while a second clone deriving the same evidence still reports `Held` (`internal/app/claims_test.go`).

**Enumeration** runs `git worktree list --porcelain -z` (requires git ≥ 2.36, named in every failure message) and parses NUL-separated fields: `worktree <path>` opens a record; recognized attributes are `branch` (with `refs/heads/` trimmed; empty for detached HEAD), `prunable`, `bare`, and `locked`; unknown keys are ignored (`internal/workspace/checkouts.go`). `locked` is read but is deliberately not a vote on liveness — git already withholds `prunable` from a locked record, so dropping the record on `locked` would be a second vote on the same fact — and it leaves the enumeration as `Checkout.Locked`, becoming the `claims.Locked` presence the routing relation and the takeover gate both read (`internal/app/claims.go`, `internal/claims/local.go`). Records that are `prunable` or `bare` are dropped as uninhabited — git's judgment, which correctly handles both an `rm -rf`'d worktree (prunable despite its leftover git dir) and a locked worktree on removable media (not prunable despite a missing directory) (`checkouts.go`). Zero records is an error (git always lists the current worktree; reporting zero would void every claim), and any per-checkout read failure aborts the whole enumeration rather than silently asserting a checkout is deleted (`checkouts.go`). Checkouts without a minted token are then dropped: a never-mutated checkout carries no token, holds no claim, and voids nothing (`internal/app/claims.go`).

## Derivation: the four-legged predicate

`Derive(evidence, fresh, local)` computes a standing per lane; it writes nothing (`internal/claims/derive.go`). Per lane, `standingOf` runs four legs in dependency order **1, 4, 2, 3** (`derive.go`):

**Leg 1 — the lane is unfinished.** If no member is in play (`InPlay` = not archived/deleted and not closed; see `01-data-model.md`), the lane is `Unclaimed` — a claim on finished work does not exist (`derive.go`). An all-closed lane and a lane whose sole ticket is archived both derive `Unclaimed`.

**Leg 4 — the holder is live as far as this machine can tell.** Events void under `LocalCheckouts` are filtered out *before* asking who established latest — over a clone of the event slice, so one derivation cannot strip events from shared evidence and change a second derivation's answer (`derive.go`). The ordering matters: filtering first means a voided newer `start` falls through to whoever else has standing (the lane reverts to an older establisher's claim) rather than reading unclaimed (`derive.go`).

**Leg 2 — the holder produced the latest establishing event.** `LatestEstablisher` scans the admissible events for the newest one that takes or transfers the lane, regardless of attribution (`establish.go`); with none at all, the lane is `Unclaimed` (`derive.go`). An establishing event with no attribution belongs to the public checkout — `model.Attribution`'s zero value — and holds the lane exactly like any other holder: derivation stops at the latest establisher rather than scanning back to an older one, because that older event is positively known to be superseded (`derive.go`). (Contrast: a *void* event is disproven rather than superseded, and is filtered out at leg 4 before leg 2 ever runs.) A repository whose whole history predates attribution derives every lane `Held` by the public checkout, subject to freshness — the public checkout is a real holder, not a way of reading "nobody claimed it." Tenure is assembled from the establisher's timestamp and the holder's last activity of any kind in the lane (`derive.go`; `trails` at `derive.go`).

**Leg 3 — the claim is fresh.** If the freshness window does not cover `LastActivity` and the holder's worktree is not `Locked`, the standing is `Unclaimed`; otherwise `Held` with contest annotations (`derive.go`). Because freshness reads `LastActivity`, ordinary commentary or a bare field edit carries a claim through a long stretch: a `start` 80 hours ago plus an edit 30 minutes ago is `Held` under a 24-hour window. A lock on the holder's worktree is the one leg-4 finding that carries a claim past the window: `git worktree lock` is the holder speaking rather than a timer, so a locked holder's lane stays `Held` however far its clock has run (`derive.go`). Mere presence of the worktree does not: an expired claim whose holder's tree is merely on disk is `Unclaimed` like any other.

**Contest.** Contestants are checkouts *with an establishing act of their own* in the lane (a drive-by comment or grooming edit never contests), excluding the holder and any candidate whose own last activity aged out of the window — the public checkout is not excluded, and contests on the same terms as any identified checkout. Sorted most-recently-active first, tie-broken by stream string; empty (non-nil) when nobody contests (`derive.go`). Contest is an annotation, not a state: routing is unaffected and the holder remains the holder (`internal/claims/standing.go`). Events from foreign workspaces are never pruned by local liveness, so a foreign holder stays `Held` even when this machine enumerates zero live streams.

Summary of the legs (all under a 24 h window; from the pinned test grid):

| Dropped leg | Fixture | Result |
|---|---|---|
| none | `start` by A at −2 h, both streams live | `Held{A}` |
| 1 | all tickets closed, or sole open ticket archived | `Unclaimed` |
| 2 | only `reopen`/`archive`/`close`/bare edits | `Unclaimed` |
| none | A's `start`/`done` at −3 h/−2 h, unattributed `start` at −1 h | `Held{public}`, contested by A |
| 3 | `start` −72 h, edit −48 h | `Unclaimed` |
| none (lock) | `start` −72 h, edit −48 h, A's worktree locked | `Held{A}` |
| 4 | `start` by A, A's checkout no longer live | `Unclaimed` |

## Gathering the claim context in the CLI

`gatherClaimContext` assembles everything a command needs (`internal/cli/claims_context.go`): load config; list issues with **both** `IncludeArchived` and `IncludeDeleted` set (an establishing event can sit on a deleted or archived issue, and evidence assembly refuses partial reads); fetch parents; list all events; build evidence; enumerate live checkouts — on enumeration failure it prints `warning: could not enumerate local checkouts (…) — claim liveness check and local addresses skipped, freshness alone governs` and proceeds with the zero `LocalCheckouts`; derive standings with `Now = time.Now()`; and compute `self` as `NewAttribution(stream, workspaceID)` — a never-minted stream collapses to the zero attribution, which reads as "no live claims" with no special branch (`claims_context.go`). It also builds an `addresses` map from attribution to live local checkout (path + branch) that never reaches the shared database and lives only for the process (`claims_context.go`).

Callers: `lit next`, the backlog/workable runner, `lit start`'s authorization, and the sync-reconcile contest report.

## Gates: where claims change behavior

### `lit start` — the takeover gate (the only write gate)

`start` is the only transition with an authorization hook; it runs after the action is built and before the store apply, can abort the transition, and returns the line the start owes after the apply — the transfer notice, or nothing — decided on the lane standing it just read, so the notice needs no second gather (`internal/cli/cli.go`). The issue's lane standing and the caller's own attribution decide the requirement through `relationOf` (`internal/cli/claims_takeover.go`), the one place a standing is read against an identity:

| Standing | Condition | Relation | Requirement |
|---|---|---|---|
| `Held` | held by self (`self.Present() && By == self`) | `laneOurs` | none |
| `Held` | held by another, including the public checkout | `laneHeldForeign` | fresh-confirm |
| `Unclaimed` | — never started, finished, or the claim expired | `laneUnclaimed` | none |

A checkout with no minted token never reads "held by self," even for a lane the public checkout itself holds — `self.Present()` is false, so the held-by-another row applies instead (`claims_takeover.go`). There is no row for an expired claim because there is no standing for one: a lane whose claim has aged out is `Unclaimed` to the gate exactly as it is to routing, and `lit start` on it prints nothing about the claim that used to be there (links-claims-y6yz). The "stale-informed" requirement that once occupied that row — proceed, but print the lapsed holder's claim line and an advisory to check for unmerged work — is gone with it.

- **None**: proceed; the happy path costs one extra evidence gather and nothing else (`authorizeStart`, `claims_takeover.go`).
- **Fresh-confirm**: with `--take`, at a terminal or not, prints `… — taking over (--take)` and proceeds. Without it, a non-interactive stdout refuses (`… — this lane is claimed and active; pass --take to confirm the takeover`), and an interactive terminal prompts `take over this lane? [y/N]` reading stdin; any answer whose trimmed lowercase form starts with `y` proceeds, anything else fails with `takeover declined` (`confirmFreshTakeover`, `claims_takeover.go`). The `--take` flag's help: "Confirm taking over a lane another checkout claims right now (required for non-interactive callers; without it an interactive terminal is prompted instead)" (`cli.go`).

The transfer notice — `claim transferred: <old> -> <new>` — prints after the apply only when the lane was held when `start` looked, ours or another's, and the ticket's recorded claimant changed: `authorizeStart` asks `transferNotice` for it only from a held lane (`internal/cli/claims_takeover.go`, `internal/cli/claims_context.go`). A start on a lane nobody holds announces no transfer, whatever the row's history records about who once started it: an expired claim transfers nothing.

Proven over two real clones and a git remote: the second clone's plain `start` fails naming `--take` and `claimed`; with `--take` it succeeds printing "taking over"; a subsequent `start` on the now-transferred lane prompts nothing (`internal/cli/claims_takeover_e2e_test.go`). With the freshness window forced to 1 ms so the first clone's claim has expired, the second clone's plain `start` succeeds and prints none of `claimed`, `stale`, `check for unmerged`, `take`, or `claim transferred` (`TestStartOnAnExpiredForeignClaimIsSilent`).

### `lit next` — claim-aware routing (a read gate)

`next` routes over rows in composite-rank order to one of seven sealed outcomes (`internal/cli/next_route.go`). It is registered `app.AccessRead` (`internal/cli/register.go`) and writes nothing: it claims no lane and starts no ticket, so every line it prints either reports a state that already holds or is advice about a command the reader has yet to run (`internal/cli/next.go`).

| Outcome | Carries | Meaning |
|---|---|---|
| `ServedFromClaim` | `Row` | a startable ticket in a lane this checkout already holds |
| `ResumedOwnWork` | `Row` | a ticket already in flight in a lane this checkout holds, handed back — announced in one of two sentences, below |
| `ServedFromEpicLane` | `Row`, `Lane model.LaneID` | a pick from a different lane of an epic this checkout already holds a lane in |
| `ServedFromNewLane` | `Row`, `Lane model.LaneID` | a pick in a lane this checkout does not hold — from the global pool (step 4) |
| `ServedFromDependency` | `Row`, `Lane model.LaneID`, `Gates string` | an on-path dependency (step 1b): a pick outside our lanes that unblocks one of our own rows, `Gates` naming the row it unblocks |
| `Exhausted` | `Epics`, `Blocked` | the checkout's own epic(s) have open work, none of it reachable; returned as an error |
| `NoWork` | `Unreachable` | the global pool produced nothing; returned as an error |

`ServedFromEpicLane` carries exactly `Row` and `Lane`; there is no `Epic` field, because the epic is `Lane.Epic()` and storing it beside the lane was two clocks for one fact. `ServedFromNewLane` likewise carries a `model.LaneID`, not a pre-rendered string.

**Precedence** (`routeNext`, `next_route.go`). `ownScope` (`next_route.go`) derives the checkout's own lanes and epics from the **standings**, not from the gathered rows: rows are already narrowed by `--type/--labels/--assignee`, and deriving ownership from them let a display filter empty the set and drop the whole self-aware branch. A checkout holding at least one lane tries, in order: **step 1**, its own lanes, accepting `{serveWork, resumeWork}` and yielding `ResumedOwnWork` or `ServedFromClaim`; **step 1b**, an on-path dependency — a row outside our lanes that gates one of them (`onPathDependency`, `next_route.go`) — yielding `ServedFromDependency`, since starting it would establish a claim on a lane we do not hold, and `Gates` carries the blocked row it unblocks so the pick can say what it is for; **step 2**, the rest of our epic in lanes we do not already hold, accepting `serveWork` and yielding `ServedFromEpicLane`; **step 3**, `Exhausted`, which is terminal and never falls through to the global pool. A checkout holding no lanes starts instead at **step 4**, the focus-scoped global pool (`--all` routes over the whole queue), which accepts `serveWork` and yields `ServedFromNewLane`, otherwise `NoWork` — whose `Unreachable` carries both the rows the walk passed over and the rows the scope withheld, because "nothing is startable" and "nothing on your focus path is startable" are different answers.

**Admission** is `capacityFor` (`next_route.go`), reading the row's lifecycle state, the lane's relation to this checkout (`relationOf`, `internal/cli/claims_takeover.go`), and — for a row not yet started — its readiness classification, and returning one of three capacities (`next_route.go`): `routeAround`, `serveWork`, `resumeWork`. `relationOf` matches a lane's holder to `self` only when `self.Present()` — a checkout with no minted token is never `laneOurs`, even for a lane the public checkout itself holds, because a zero `self` equal to a zero holder proves nothing about whose lane it is. The table: `laneHeldForeign` is `routeAround` whatever the row; a row not in progress is `serveWork` when ready and `routeAround` otherwise, in any lane the checkout may act in; an in-progress row is `resumeWork` in a lane this checkout holds and `serveWork` in a lane nobody holds, because an in-flight row in an unheld lane is abandoned by definition — whoever started it no longer holds a claim there. The orphan annotation (the row's own quiet clock) does not enter routing; `lit backlog` and `lit orphaned` still read it as a description of the row. A fourth capacity, `takeoverWork`, once marked a pick that displaced a lapsed claim or abandoned in-flight work; no consumer distinguished it from `serveWork`, and with lapsed claims gone from the type it was removed (links-claims-y6yz). `accept` is a set and never a preference order: composite rank is the only tiebreak routing applies, and ranking capacities against each other would reintroduce the symptom the set fixed — the backlog's #1 row, abandoned in flight, passed over for a lower-ranked leaf that was merely ready.

Servability does not require `status == open`. Step 1 accepts `resumeWork`, so an in-progress row in the checkout's own lane is handed back to resume; while routing gated servability on `model.StateOpen`, an `in_progress` row was servable to nobody, which hid every orphan and the very ticket the checkout was working at that moment.

`Exhausted` and `NoWork` implement `error` and travel outward as themselves rather than being rendered into a generic error, which is what keeps the exit-code and reason sinks reading the routing verdict instead of a copy that could drift. Both exit **6** (`ExitNoWork`, `internal/cli/exit.go`), with reasons `scope_exhausted` and `no_ready_work` (`internal/cli/error_output.go`). Six rather than `ExitGeneric`, because a caller looping `lit next` has to tell "stop, there is nothing for you" from "lit is broken" without parsing the English; not `ExitOK`, because for `lit next` 0 means a ticket is on stdout, and exiting 0 with no row would hand the caller a success-shaped void.

Both diagnostics are written in `reachKind`, which says what one row is to this checkout right now: `reachTakeable`, `reachHeldFresh`, `reachNotReady`, `reachOutOfView`, and `reachOffFocusPath`, the last used only by the pool diagnostic. A bool here read "takeable or not", so a row outside the run's filtered view, or one not startable itself, rendered as the one reason the message named: claimed by another checkout. Exhaustion asks `reachKind` of the dependencies gating our scope; an empty global pool asks it of every row the walk went past. Each clause names at most twelve ids and says how many it left out (`maxNamedPerKind`, `next_route.go`); the per-kind wordings and both error formats are in inventory-claims.md §9.2.

After routing, `next` prints the advice line above any pick that would establish a claim — naming what running `lit start` would lock rather than what `next` did, since reporting an act is the one thing a read-only command must not do — then the ticket summary with its claim line, and dispatches the pulled-ticket workflow occasion (`next.go`, `startAdvice` at `next.go`).

`ResumedOwnWork` is the one outcome whose announcement is not `startAdvice`'s: it reports a state rather than advising a command, and it is written by `resumeAdvice` (`next.go`), which chooses between two sentences on one question — does the row's assignee name somebody other than the identity running this command? When it does, the line names them: ``<id> is in progress and assigned to <assignee>, not to you — check that they have stopped before you continue it, or take other work from `lit backlog` ``. Otherwise it is the sentence `next` has always printed, `<id> is already in progress in a lane you hold — continue where you left off`. An empty assignee is the only thing that silences the first — the ordinary state of a ticket started by a checkout driving no agent session, where there is no name to contradict; an unidentified reader is warned like any other, because nobody's name is the empty string.

The split exists because the lane cannot carry the claim the old sentence made. A lane is keyed on the checkout, so every session running in one checkout holds it — that is what lets a fresh session inherit its predecessor's work with no re-briefing — and it is also what let one of two sessions running at once be told it had been working a ticket the other was mid-PR on (links-routing-t6fa). The assignee is the finer fact the lane never carried. Nothing here adjudicates whether the named holder is still running: every session mints a new identity, so a predecessor and a live peer both read as "not you", and claims carry staleness heuristics with no liveness probe by design (see "Finding a claimant" above). The orphan clock is not that discriminator either — `updated_at` moves on field writes and transitions, not on the work — so the line names the holder, asks for the check, and leads with continuing rather than dropping the work.

The reader's identity travels to `renderNextOutcome` as an argument, not on `claimContext` — that context is gathered by four commands and read for its standings, and an identity living there would be produced once for every caller and consumed by one. `next` resolves it through the hidden `--by` fallback every mutating command registers (`next.go`), and is the only read command that does: the assignee it is compared against is written by the same `resolveIdentity`, so resolving the reader from the session environment alone would compare two halves settled by different rules. Its own `--assignee` narrows the view and is never read as an identity.

### `lit sync reconcile` — the contest report

After a reconcile whose outcome actually merged histories (linearized or combined — not prose-pending, unrelated-histories, or not-diverged), lit reports lanes where evidence from more than one checkout just met (`internal/cli/sync_reconcile_cmd.go`). The report lists every `Held` lane with a non-empty contested set, sorted by lane string, under the header `contested: evidence from more than one checkout just met for these lanes —`, each with its claim line (`internal/cli/claims_contest_report.go`). The gather runs without a `Stream`, so `self` is zero for this call. Nothing is printed when no lane is contested.

### Render-only surfaces

`lit backlog` prints the claim line (indented, between the `in_progress:` and `unblocks:` lines), and `lit next`'s summary block prints it between `depends on` and `unblocks` (`internal/cli/backlog.go`, `internal/cli/ready_state.go`). No other command consults standings; every other consumer of the claim context uses it only for rendering.

## Rendering the claim line

`formatClaimLine` renders `Held` only — an `Unclaimed` lane renders no line at all, and a lane whose claim has expired is `Unclaimed`, so nothing is printed about the claim that used to be there even when its holder's worktree is still on this machine and resolvable (`internal/cli/claims_render.go`; pinned by `TestFormatClaimLineExpiredClaimRendersNothingEvenWithAnAddress`). The line joins with ` · `: the holder badge, the coarse age of `LastActivity`, and lane progress when the lane has members.

- **Holder badge** (`claimPrefix`, `claims_render.go`): if the holder resolves to a live local worktree, `claimed here: <path> (<branch>)` — branch shown as `detached HEAD` when empty. Otherwise `claimed: <name> (<state>)`, where `nameCheckout` (`claims_render.go`) gives the stream's short token or, for the public checkout, the literal `the public checkout`, and `holdState` (`claims_render.go`) gives `elsewhere` (an identified holder) or `unaddressed` (the public checkout, which has no address to compare against). There is no `(stale)` or `(locked)` tag: a locked worktree past the clock is an ordinary `Held` lane and reads as one.
- **Contest suffix** on a contested `Held`: ` · contested by <short-streams>`.
- **Lane progress** (`claims_render.go`): `""` for an empty lane; `<active-id> in progress, <done>/<total> done` when a member is in progress; else `<done>/<total> done`.
- **Short streams**: tokens truncated to the first 8 characters for display — a nicety, not a privacy measure, since the full token is already opaque (`nameCheckout`, `claims_render.go`).
- **Coarse durations** (`internal/cli/output.go`): ≥ 48 h → `N days`; ≥ 2 h → `N hours`; ≥ 2 m → `N minutes`; else `under a minute`.

The two tiers are deliberate: the dossier (holder, freshness, progress) comes entirely from shared synced data and renders identically on any clone; the local path/branch renders only on the machine that enumerated the holder's worktree (`claims_render.go`).

## The `internal/app` service layer

`internal/app` is the seam between the CLI and the store: two files, and this complete surface (`internal/app/app.go`, `internal/app/claims.go`).

- **`App`** — `{Workspace workspace.Info; Store storage.Store; Stream workspace.StreamID}`. `Stream` is always present under write access (minted on the checkout's first mutating command); present under read access only if an earlier mutation minted it — absence is the honest report that the checkout holds no claim (`app.go`).
- **`Open(ctx, cwd, mode)`** — in order: validate the mode by table lookup (unknown, including `""`, → `invalid access mode`); resolve the workspace from cwd; open the store engine (write mode bootstraps a missing database; read mode fails with "not initialized"); resolve the stream identity *after* the store opens, so a command that cannot reach its store mints nothing; on identity failure close the store (which also releases the workspace lock) and join both errors; call `AttributeTo` unconditionally (`app.go`). A malformed token file fails the open with a "malformed" diagnosis and releases the store — proven by a repaired second open succeeding.
- **`OpenLocationForRead(ctx, loc)`** — opens a store at an already-derived location, bypassing cwd git resolution; the cross-project primitive for aggregating over many stores. Reads the foreign store's `workspace_id` from its own `config.json` (a pure read), always opens read-only (so the foreign store gets the shared lock, never a second read-write engine the embedded driver would reject), mints no identity, and stamps nothing (`app.go`).
- **`Close()`** — `Store.Close()` (`app.go`).
- **`LocalCheckouts()`** — enumerates live checkouts and scopes them to this workspace id; on error returns the zero value *and* the error — it reports what it proved or that it proved nothing (`claims.go`).

The package emits no events and publishes no observer surface.

## Privacy invariants

- Both halves of an attribution are opaque by mandate; nothing user-, host-, or path-shaped travels there, because the database syncs to shared remotes. Resolving a token to a physical checkout happens only on the machine that owns it (`internal/model/model.go`).
- The stream token is deliberately meaningless — no directory, hostname, or username material (`internal/workspace/stream.go`).
- The `--by` fallback is `""` (normalized to `"unknown"`), the old `$USER` default having been removed as an invariant violation (`internal/cli/cli.go`).
- Checkout paths and branches stay on the local machine; the address map lives only for the process (`internal/workspace/checkouts.go`, `internal/cli/claims_context.go`).
- A different clone on the same machine has a different workspace id, so this machine's enumeration never speaks to its claims (`internal/app/claims.go`).
