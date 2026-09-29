# Behavioral inventory — claims / assignment subsystem

All paths relative to `/Users/bmf/code/links-issue-tracker`. Every claim below carries a `file:line` citation. Derived from Go source only.

---

## 1. What a claim IS

### 1.1 Nothing is stored

A claim is a *read-time derivation* over records the database already holds. `internal/claims/evidence.go` states the package "derives, at read time, which checkout is working which lane. Nothing here is stored." The package imports only `cmp`, `fmt`, `slices`, `time`, `strings`, and `internal/model` (`internal/claims/evidence.go`, `internal/claims/derive.go`); it reads no clock, no store, no filesystem — the clock reading arrives as data (`internal/claims/derive.go`).

There is no claims table, no claim row, no claim file. The only persisted footprint is `model.Attribution` stamped on each `model.IssueEvent` (`internal/model/model.go`).

### 1.2 The persisted primitive: `model.Attribution`

```go
type Attribution struct {
	stream    string
	workspace string
}
```
`internal/model/model.go`.

- Both halves unexported; `NewAttribution` is the only constructor (`internal/model/model.go`).
- `NewAttribution(stream, workspace)` returns the zero value if **either** half is empty — "complete pair or nothing" (`internal/model/model.go`). The collapse is silent by design (`internal/model/model.go`).
- Accessors: `Stream()`, `Workspace()` (`internal/model/model.go`); `IsZero()` = `a == Attribution{}` (`internal/model/model.go`); `Present()` = `!IsZero()` (`internal/model/model.go`).
- Wire form is a separate struct `attributionWire{Stream string \`json:"stream,omitempty"\`; Workspace string \`json:"workspace,omitempty"\`}` (`internal/model/model.go`), marshalled at `internal/model/model.go`.
- `UnmarshalJSON` routes through `NewAttribution`, so `{"stream":"x"}` with no workspace decodes to the absent pair (`internal/model/model.go`).
- Absence is a permanent legal state: attribution is never backfilled onto events that predate the feature (`internal/model/model.go`).
- Field on the event: `Attribution Attribution \`json:"attribution,omitzero"\`` (`internal/model/model.go`) — `omitzero` consults `IsZero`, so an unattributed event writes no attribution object at all (`internal/model/model.go`).

### 1.3 The derived value: `Standing`

Sealed sum with two variants, discriminated by unexported marker `isStanding()` (`internal/claims/standing.go`):

- `Unclaimed struct{}` — no holder: never started, finished, or the claim's evidence has aged out (`internal/claims/standing.go`).
- `Held struct { Tenure; Contested []model.Attribution }` (`internal/claims/standing.go`).

There is no variant for an expired claim. `Stale struct { Tenure; Holder Presence }` existed until links-claims-y6yz and was removed: an expired claim is not a claim, and the type has nowhere to record that one existed.

`Tenure`:
```go
type Tenure struct {
	By           model.Attribution
	Since        time.Time
	LastActivity time.Time
}
```
`internal/claims/standing.go`. `Since` = timestamp of the establishing event that put the holder there; `LastActivity` = the holder's most recent mutation of any kind in the lane, and freshness is measured against `LastActivity`, not `Since` (`internal/claims/standing.go`).

`Standings map[model.LaneID]Standing` with total read `Of(lane)`: a missing key returns `Unclaimed{}` rather than a nil interface (`internal/claims/standing.go`).

### 1.4 The unit a claim is held over: `model.LaneID`

```go
type LaneID struct { epic string; key string; solo bool }
```
`internal/model/model.go`. Fields unexported; `LaneOf` is the only constructor (`internal/model/model.go`).

`LaneOf(issue, parent)` (`internal/model/model.go`):
- `parent == nil` **or** `!parent.IsContainer()` → `LaneID{key: issue.ID, solo: true}` (a "lane of one").
- otherwise → `LaneID{epic: parent.ID, key: issue.Lane}`. An epic that declares no lanes is exactly one lane keyed by `""` (`internal/model/model.go`).

`Epic()` / `Key()` accessors at `internal/model/model.go`. `String()`: solo → bare key; otherwise `epic + "#" + key`, so an epic's unnamed default lane renders `"E#"` (`internal/model/model.go`).

Test coverage of the three shapes: epic-major lanes `TestLaneGranularity` (`internal/claims/claims_test.go`), cross-epic `TestCrossEpicDependencyClaimsOnlyItsOwnLane` (`internal/claims/claims_test.go`), solo `TestParentlessTicketIsItsOwnLane` (`internal/claims/claims_test.go`).

---

## 2. Identity derivation

There are **two distinct identity systems**, and they do not share a value:

| system | value | where it lives | who reads it |
|---|---|---|---|
| checkout identity (claims) | `workspace.StreamID` + workspace id | `<private git dir>/lit-stream` (local only) | claim derivation |
| acting identity (assignee/actor strings) | `claude_<session>` / flag value | flags + `CLAUDE_CODE_SESSION_ID` | `Issue.Assignee`, `IssueEvent.Actor`, `CreatedBy` |

### 2.1 Checkout identity — `StreamID`

- `type StreamID struct{ value string }`, unexported field so no arbitrary string can become one (`internal/workspace/stream.go`). `Value()`, `Present()` = `value != ""`.
- Zero value means "this checkout has never minted a token" and is a legitimate state, not an error (`internal/workspace/stream.go`).
- Storage file name: `lit-stream` (`internal/workspace/stream.go`), stored in the checkout's **private** git dir (`--git-dir`), never the common dir — so every worktree of a repo shares one backlog but carries a distinct token, and `git worktree remove` deletes the token with the directory (`internal/workspace/stream.go`).
- Entropy: 8 bytes from `crypto/rand` (`internal/workspace/stream.go`), unpadded base32 (`internal/workspace/stream.go`), lowercased → alphabet `[a-z2-7]`, length `(8*8+4)/5 = 13` characters (`internal/workspace/stream.go`).
- File content is the token plus a trailing newline (`internal/workspace/stream.go`); file mode set explicitly to `0644` (`internal/workspace/stream.go`).

**`ReadStream(privateGitDir)`** (`internal/workspace/stream.go`):
- Missing file → zero `StreamID`, nil error.
- Any other read error → wrapped error `read stream id %q: %w`.
- Present file → `parseStreamToken`.

**`parseStreamToken`** (`internal/workspace/stream.go`): trims whitespace; rejects anything not exactly 13 chars ("expected %d characters, found %d") and any character outside `a-z`/`2-7` ("character %q is outside the token alphabet"). Error text always appends the remedy: `delete the file to mint a fresh identity for this checkout (work already recorded under the old identity keeps it, and any lane this checkout held is released)` (`internal/workspace/stream.go`). Never self-heals (`internal/workspace/stream.go`).

**`EnsureStream(privateGitDir)`** (`internal/workspace/stream.go`): read-first fast path; if absent, `publishStreamToken`, then re-read; if still absent after publishing → error `stream id %q vanished immediately after it was written`. The FILE, never the freshly-minted candidate, decides identity (`internal/workspace/stream.go`).

**`publishStreamToken`** (`internal/workspace/stream.go`): mints token → `os.CreateTemp` in the same directory → write + `Chmod(0644)` + `Sync` + `Close` → `os.Link(temp, final)`. `os.ErrExist` from the link is a **success** (a racing caller already published) (`internal/workspace/stream.go`). Any other link failure produces an error naming the hard-link requirement explicitly (`internal/workspace/stream.go`). Temp file removed via `defer` on all paths (`internal/workspace/stream.go`). The directory entry is deliberately not fsynced (`internal/workspace/stream.go`).

### 2.2 Which commands mint

`app.AccessMode` is `"read"` or `"write"` (`internal/app/app.go`). The mapping table:

```go
var accessContracts = map[AccessMode]accessContract{
	AccessRead:  {mode: engine.ReadOnly, resolveStream: workspace.ReadStream},
	AccessWrite: {mode: engine.ReadWrite, resolveStream: workspace.EnsureStream},
}
```
`internal/app/app.go`. Store access mode and identity minting are paired in one value so they cannot disagree (`internal/app/app.go`).

Behavioral consequences pinned by test: a write-mode open mints; a read-mode open in a never-mutated checkout mints nothing, and a *second* read still finds nothing (`internal/app/app_test.go`); a read-mode open after a write sees exactly the minted token (`internal/app/app_test.go`); two worktrees never share an identity (`internal/app/app_test.go`).

### 2.3 The pair reaching the database

`app.Open` calls `st.AttributeTo(stream.Value())` unconditionally for both modes (`internal/app/app.go`). `Store.AttributeTo` pairs the raw token with the store's own workspace id: `s.attribution = model.NewAttribution(streamToken, s.workspaceID)` (`internal/store/store.go`). An empty token leaves the store unattributed rather than half-attributed (`internal/store/store.go`). Stamping happens at `recordEvent`, the single insertion point for issue history (`internal/store/store.go`). `app.Open` is the only caller of `AttributeTo`; `OpenSync`, `RebuildCandidate`, adopt, upgrade, and `OpenLocationForRead` do not stamp — they read, or replay dumps through `insertEventTx`, which preserves the producer's attribution (`internal/store/store.go`). The interface is `storage.Attributor` (`internal/storage/contract.go`).

Workspace id source: `Info.WorkspaceID` (`internal/workspace/workspace.go`), read from config (`internal/workspace/workspace.go`), generated as a UUID at init (`internal/workspace/workspace.go`).

Cross-clone proof: attribution survives a real git-remote round trip and a second clone sees the producer's exact pair, never re-stamped (`internal/cli/claims_attribution_test.go`).

### 2.4 Acting identity — `resolveIdentity` (assignee and event actor)

```go
func resolveIdentity(explicit string) string {
	if sessionID := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")); sessionID != "" {
		return "claude_" + sessionID
	}
	return strings.TrimSpace(explicit)
}
```
`internal/cli/cli.go`.

Precedence, exactly: **`CLAUDE_CODE_SESSION_ID` (trimmed, non-empty) always wins** and yields `"claude_" + sessionID` regardless of any flag; otherwise the caller's explicit value, trimmed; otherwise `""` (`internal/cli/cli.go`).

- Assignee flag on `start`: `--assignee`, help string `"Assignee fallback when CLAUDE_CODE_SESSION_ID is unset (env always wins when set)"` (`internal/cli/cli.go`). Action built as `model.Start{Assignee: resolveIdentity(*assignee)}` (`internal/cli/cli.go`).
- Actor flag: hidden `--by`, empty default, registered by `registerActor` which never exposes the raw pointer — every read passes through `resolveIdentity` (`internal/cli/cli.go`). `os.Getenv("USER")` was deliberately removed as a privacy violation; the fallback is `""`, normalized by the store to the opaque `"unknown"` (`internal/cli/cli.go`).
- Applied in `transitionLeaf`: `actor := resolveActor()` then `Store.Apply(ctx, issueID, storage.Change{Action: action, Actor: actor, Reason: *reason})` (`internal/cli/cli.go`).
- Every relation/label/bulk verb resolves through the same rule; tests pin `label add`, `parent set`, `dep add`, `bulk label add`, `bulk close` to `"claude_" + sessionID` and assert raw `$USER` never lands in `CreatedBy`/`Actor` (`internal/cli/attribution_test.go`, helpers).
- A claimant with no assignee is named by its checkout: `describeClaimant` returns `nameCheckout(c.Checkout)` when `c.Assignee == ""` (`internal/cli/claims_render.go`).
- `Start` is the only lifecycle action that rewrites the assignee: `type Start struct{ Assignee string }` (`internal/model/lifecycle/action.go`), `Target() State` = `InProgress` (`internal/model/lifecycle/action.go`). `Issue.Assignee` is orthogonal to the status machine (`internal/model/model.go`).

Note the interaction pinned by test: because the env var overrides `--assignee`, an e2e test must clear `CLAUDE_CODE_SESSION_ID` or both checkouts flatten to one assignee and a same-state `start` becomes a documented no-op in `store.Apply`, so the claim never transfers (`internal/cli/claims_takeover_e2e_test.go`).

---

## 3. Evidence assembly

### 3.1 `Evidence`

```go
type Evidence struct {
	members map[model.LaneID][]model.Issue
	events  map[model.LaneID][]model.IssueEvent
}
```
`internal/claims/evidence.go`.

**`NewEvidence(issues, parents, events)`** (`internal/claims/evidence.go`):
1. For each issue: `lane := model.LaneOf(issue, parents[issue.ID])`; record `lanes[issue.ID] = lane`; append to `members[lane]`. An issue absent from `parents`, or mapped to nil, is parentless.
2. For each event: look up `lanes[event.IssueID]`. If unknown → **error**, verbatim: `claims: event %s belongs to issue %s, which was not among the %d issues supplied: claim derivation needs every issue the events touch, closed ones included` (`internal/claims/evidence.go`). Rationale: a `done` on a now-closed ticket can be the sole establishing act. Pinned by `TestEvidenceRefusesAPartialRead` (`internal/claims/claims_test.go`).
3. Sort each lane's events by `cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))` — timestamp first, id as the tiebreak; total and stable (`internal/claims/evidence.go`). Order-independence of the input pinned by `TestDeriveIsOrderIndependent` (`internal/claims/claims_test.go`).

`Lanes()` returns every lane in the members map, unordered (`internal/claims/evidence.go`).

### 3.2 `LaneProgress`

```go
type LaneProgress struct { Done, Total int; Active *model.Issue }
```
`internal/claims/evidence.go`.

`Evidence.LaneProgress(lane)` (`internal/claims/evidence.go`): iterate `members[lane]`; `Total++` for every member; `Done++` when `issue.State() == model.StateClosed`; `Active` set to a copy of the member whose state is `model.StateInProgress` (last such member wins, since it is assigned unconditionally in the loop). A lane the evidence never saw returns the zero value (`Total 0`), matching `Standings.Of`'s totality convention (`internal/claims/evidence.go`).

Pinned: closed+in-progress+open lane → `{Done:1, Total:3, Active:T2}` (`internal/claims/evidence_progress_test.go`); no in-progress member → `Active == nil`; unseen lane → zero.

### 3.3 What counts as an establishing act

```go
var establishing = map[model.ActionName]bool{
	model.ActionStart: true,
	model.ActionDone:  true,
	model.ActionClose:     false,
	model.ActionReopen:    false,
	model.ActionArchive:   false,
	model.ActionUnarchive: false,
	model.ActionDelete:    false,
	model.ActionRestore:   false,
}
```
`internal/claims/establish.go`.

- **Exactly two verbs establish**: `start` and `done` (`internal/claims/establish.go`). `start` is the act of taking work; `done` is the neutral success close, so a checkout that just completed a ticket mid-lane still holds the lane (`internal/claims/establish.go`).
- `close` (which carries an Outcome: duplicate/superseded/obsolete/wontfix), `reopen`, and the four retention verbs never establish (`internal/claims/establish.go`).
- `establishes(event)` looks up `establishing[model.ActionName(event.Action)]`; an empty `Action` (plain field update) and an unrecognized verb both read false through the same lookup (`internal/claims/establish.go`). Pinned by `TestAbsentVerbDoesNotEstablish` (`internal/claims/establish_internal_test.go`).
- A map rather than a switch so `TestEstablishingCoversEveryAction` can assert every verb in `model.Actions()` is classified and that the map names no retired verb (`internal/claims/establish_internal_test.go`). `TestOnlyStartAndDoneEstablish` pins the exact classification.
- Actions vocabulary: `ActionStart = "start"` etc. (`internal/model/lifecycle/lifecycle.go`), sealed list at `internal/model/lifecycle/lifecycle.go`.

---

## 4. Freshness and the clock

```go
type Freshness struct { Now time.Time; Window time.Duration }
func (f Freshness) Covers(t time.Time) bool { return !t.Before(f.Now.Add(-f.Window)) }
```
`internal/claims/derive.go`.

- Both the clock reading and the window travel as data; the derivation reads no clock (`internal/claims/derive.go`).
- **Boundary rule: evidence exactly on the boundary is covered** — `!t.Before(Now-Window)`, i.e. `t >= Now-Window` (`internal/claims/derive.go`).
- `Covers` is the single place a timestamp is compared against the window (`internal/claims/derive.go`).

**Configuration**: `claims.freshness_window`, default `"6h"` (`internal/config/config.go`). Field `ClaimsConfig.FreshnessWindow time.Duration` with `mapstructure:"-"` — deliberately NOT struct-tag decoded (`internal/config/config.go`). Parsed once by `parseFreshnessWindow` (`internal/config/config.go`):
- `time.ParseDuration(raw)` failure → `config: claims.freshness_window must be a duration with a unit, like "72h" or "90m" (got %q): %w` (`internal/config/config.go`).
- `window <= 0` → `config: claims.freshness_window must be positive, got %s` (`internal/config/config.go`).
- The reason for string parsing: viper weak-decoding a bare `72` would land as 72 **nanoseconds**, positive and passing validation, expiring every claim instantly (`internal/config/config.go`).

**Where `Now` comes from at runtime**: `claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}` in `gatherClaimContext` (`internal/cli/claims_context.go`).

E2E manipulation of the window: a test writes `.lit/config.toml` containing `[claims]\nfreshness_window = "1ms"\n` and sleeps 50 ms (`internal/cli/claims_takeover_e2e_test.go`).

---

## 5. Local liveness

### 5.1 `claims.LocalCheckouts`

```go
type LocalCheckouts struct { workspace string; live map[string]struct{} }
```
`internal/claims/local.go`. Constructed by `NewLocalCheckouts(workspaceID, live)` (`internal/claims/local.go`).

**Zero value = "this machine has enumerated nothing and therefore proves nothing"**; it voids nothing, which is the "where uncheckable, assume live and let freshness govern" default (`internal/claims/local.go`). Callers that cannot enumerate must pass the zero value, never a guess (`internal/claims/local.go`).

**`Void(at model.Attribution) bool`** (`internal/claims/local.go`):
```go
if !at.Present() || at.Workspace() != l.workspace { return false }
_, alive := l.live[at.Stream()]
return !alive
```
So an event is void iff: attribution present **and** its workspace equals this machine's workspace **and** its stream token is not in the live set. An unattributed event can never be void, which also keeps the zero `LocalCheckouts` inert (its empty workspace would otherwise match an absent pair's empty workspace) (`internal/claims/local.go`).

Asymmetry, stated at `internal/claims/local.go`: worktree deletion is a local fact; a claim from a deleted checkout dies here at once, everywhere else waits out the freshness window; a different clone on the same machine carries a different workspace id and is never pruned.

Pinned by `TestLocalCheckoutsScopesTokensToThisWorkspace`: a token of *this* workspace belonging to no live checkout is void; the same token under another workspace id is not; the current checkout's own pair is not (`internal/app/claims_test.go`).

### 5.2 Enumeration — `workspace.LiveCheckouts`

```go
type Checkout struct { Stream StreamID; Path string; Branch string }
```
`internal/workspace/checkouts.go`. `Branch` empty for detached HEAD (`internal/workspace/checkouts.go`). Nothing is stored; the value is re-derived per enumeration (`internal/workspace/checkouts.go`).

`LiveCheckouts(cwd)` (`internal/workspace/checkouts.go`):
1. Runs `git worktree list --porcelain -z` with `context.Background()`. On failure the error names the git ≥ 2.36 requirement on **every** failure and is deliberately not routed through `classifyGitError`.
2. `parseWorktreeList`, then `slices.DeleteFunc(records, worktreeRecord.uninhabited)`.
3. For each remaining record: `resolvePrivateGitDir(record.path)` then `ReadStream(privateGitDir)`; any failure aborts the **whole** enumeration rather than dropping the checkout, because dropping it would silently assert the checkout is deleted (rationale).

`worktreeRecord{path, branch, prunable, bare}` (`internal/workspace/checkouts.go`); `uninhabited() = prunable || bare`. A worktree deleted with `rm -rf` leaves its private git dir behind, so filesystem listing would report it alive — git reports it `prunable`; a *locked* worktree is not prunable even with a missing directory (removable media), and this inherits git's judgment (`internal/workspace/checkouts.go`).

`parseWorktreeList` (`internal/workspace/checkouts.go`): splits on `\x00`; `strings.Cut(field, " ")` on the FIRST space only; `worktree <path>` opens a record; empty field skipped; an attribute field before any `worktree` field → error `git worktree list --porcelain -z opened with %q, which is not a 'worktree <path>' field`. Recognized attributes: `branch` (with `refs/heads/` prefix trimmed), `prunable`, `bare`, `locked`; unknown keys ignored (documented). `locked` is read but deliberately does not vote on liveness — git already withholds `prunable` from a locked record — and it leaves as `Checkout.Locked`, reaching the `claims.Locked` presence via `internal/app/claims.go` and `internal/claims/local.go`. It is the signal `relationOf` reads. Zero records → error `git worktree list --porcelain -z named no worktrees at all`, because git always lists the current worktree and reporting zero would void every claim.

### 5.3 `app.App.LocalCheckouts` (the boundary)

`internal/app/claims.go`:
```go
checkouts, err := workspace.LiveCheckouts(a.Workspace.RootDir)
if err != nil { return claims.LocalCheckouts{}, err }
return claims.NewLocalCheckouts(a.Workspace.WorkspaceID, LiveCheckoutsOf(checkouts)), nil
```
On error it returns the zero value **and** the error — it reports what it proved or that it proved nothing (`internal/app/claims.go`).

`LiveCheckoutsOf(checkouts)` drops any checkout whose `Stream` is not `Present()` and projects each remaining one onto `claims.LiveCheckout{Stream: checkout.Stream.Value(), Locked: checkout.Locked}` (`internal/app/claims.go`); a never-mutated checkout carries no token, holds no claim, and voids nothing (`internal/app/claims.go`). Pinned by `TestLiveCheckoutsOfCountsOnlyMintedIdentities` (`internal/app/claims_test.go`).

---

## 6. Derivation — the four-legged predicate

`Derive(evidence, fresh, local) Standings` iterates every lane in `evidence.members` and calls `standingOf(members, events[lane], fresh, local)` (`internal/claims/derive.go`). It writes nothing.

`standingOf` (`internal/claims/derive.go`) runs the legs in dependency order **1, 4, 2, 3** (`internal/claims/derive.go`):

**Leg 1 — the lane is unfinished.** `if !slices.ContainsFunc(members, model.Issue.InPlay) { return Unclaimed{} }` (`internal/claims/derive.go`). `Issue.InPlay()` = `!lifecycle.Frozen(i.Retention()) && i.State() != model.StateClosed` (`internal/model/model.go`) — so archived and deleted issues are out of play as well as closed ones. Pinned by `TestPredicateGrid`'s "leg 1 dropped" cases: an all-closed lane and a lane whose sole open ticket is archived both read `Unclaimed` (`internal/claims/claims_test.go`).

**Leg 4 — the holder is live as far as this machine can tell.** Applied as a *filter* over events, before leg 2:
```go
admissible := slices.DeleteFunc(slices.Clone(events), func(event model.IssueEvent) bool {
	return local.Void(event.Attribution)
})
```
`internal/claims/derive.go`. The `slices.Clone` is load-bearing: `DeleteFunc` compacts in place, so without it one derivation would strip events out of the shared `Evidence` and a second derivation over the same reading would silently differ (`internal/claims/derive.go`). Pinned by `TestDeriveDoesNotConsumeItsEvidence`: one `Evidence`, derived twice — once with a pruning `LocalCheckouts` (→ Unclaimed) and once with the zero value (→ Held) (`internal/claims/claims_test.go`).

Ordering rationale: leg 4 must run before leg 2 asks which establishing event is *latest*, or a lane would read unclaimed where it should revert to whoever else has standing (`internal/claims/derive.go`). Pinned by `TestVoidEvidenceFallsThroughToTheNextEstablisher`: A's newer `start` is void, so the lane reverts to B's older one (`internal/claims/claims_test.go`).

**Leg 2 — the holder produced the latest establishing event.**
```go
establisher, found := LatestEstablisher(admissible)
if !found { return Unclaimed{} }
holder := establisher.Attribution
```
`internal/claims/derive.go`. `LatestEstablisher` is exported and lives in `internal/claims/establish.go`, not `derive.go` — it scans the whole slice for the newest event by `byRecency` for which `establishes` is true, **regardless of attribution**.

**The derivation stops at the latest establisher, attributed or not; it never scans back to an older ancestor** (`internal/claims/derive.go`). An establishing event with no attribution belongs to the public checkout — `model.Attribution`'s zero value — and holds the lane exactly like any other holder: an older attributed event is positively known to be superseded, so scanning past the newest establisher would hand the lane to a checkout that has demonstrably moved on. Pinned by `TestUnattributedLatestStopsRatherThanScanning` (`internal/claims/claims_test.go`): A starts then completes T1, then the public checkout starts T2 later — the lane reads `Held{By: public}` with A contesting, **not** `Held{By: A}`. Contrast with a *void* event, which is disproven rather than merely superseded and therefore falls through at leg 4, before leg 2 ever runs (`internal/claims/local.go`).

`trails(admissible)` folds the events into two maps (`internal/claims/derive.go`): `activity[attribution] = event.CreatedAt` (last write wins → each checkout's latest act, because events are oldest-first) and `establishers[attribution] = struct{}{}` for establishing events only.

`tenure := Tenure{By: holder, Since: establisher.CreatedAt, LastActivity: activity[holder]}` (`internal/claims/derive.go`).

**Leg 3 — the claim is fresh.**
```go
if !fresh.Covers(tenure.LastActivity) && local.PresenceOf(holder) != Locked { return Unclaimed{} }
return Held{Tenure: tenure, Contested: contestants(holder, activity, establishers, fresh)}
```
`internal/claims/derive.go`. Freshness is measured from the holder's **last mutation of any kind in the lane**, not from the establishing event, so ordinary commentary carries a claim through a long stretch (`internal/claims/derive.go`). Pinned by `TestAnyMutationRefreshes`: `start` 80 h ago plus a bare field edit 30 min ago → Held with `Since = -80h`, `LastActivity = -30m` under a 24 h window. A `Locked` holder is the one leg-4 finding that carries a claim past the window; `Present` and `Unprovable` do not.

Expired example: `start` at −72 h plus a field edit at −48 h under a 24 h window → `Unclaimed{}` (`TestPredicateGrid`'s "leg 3 dropped" case). The same fixture with streamA's worktree locked → `Held{By: streamA, Since: -72h, LastActivity: -48h}`; with it merely present, gone, or unenumerable → `Unclaimed{}` (`TestExpiredClaimPresenceGrid`).

**Contest.** `contestants(holder, activity, establishers, fresh)` (`internal/claims/derive.go`):
- Candidate set = the keys of `establishers` (so **only checkouts with an establishing act** contest; a drive-by comment or grooming edit never does — pinned by `TestDriveByEditsNeitherEstablishNorContest`, `internal/claims/claims_test.go`).
- Skip candidate if `candidate == holder`, or `!fresh.Covers(activity[candidate])` — a rival whose own evidence aged out is no longer contesting. **Unattributed candidates are not skipped**: the public checkout contests on the same terms as any identified checkout, pinned by `TestPublicCheckoutContestsAnIdentifiedHolder` (`internal/claims/claims_test.go`).
- Sort: most-recently-active first (`activity[b].Compare(activity[a])`), tie-broken by `strings.Compare(a.Stream(), b.Stream())`.
- Returns `[]model.Attribution{}` (non-nil empty) when nobody contests.
- Contest is an annotation, not a state: routing is unaffected and the holder remains the holder (`internal/claims/standing.go`).

Pinned: A starts at −3 h, B starts at −1 h → `Held{By: B, Contested: [A]}` (`TestContestedAnnotatesWithoutMovingRouting`, `internal/claims/claims_test.go`); A's start at −200 h with B at −1 h → Held by B, no contest (`TestContestLapsesWithTheRivalsEvidence`, `internal/claims/claims_test.go`).

**Cold start.** A repository whose whole history predates attribution derives every lane `Held` by the public checkout, subject to freshness. Real pre-attribution history is almost always older than the freshness window, so in practice those claims have expired and the lanes read `Unclaimed`. Pinned by `TestColdStartDerivesThePublicCheckout`: recent all-public-checkout history reads `Held{public}`; the same shape 89-90 days old under a 24 h window reads `Unclaimed`.

**Foreign workspaces never pruned**: an event from `ws-elsewhere` remains Held even when this machine enumerates zero live streams for `ws-local` (`TestForeignWorkspaceIsNeverPruned`, `internal/claims/claims_test.go`).

**Grid summary** (all under a 24 h window; `TestPredicateGrid`, `internal/claims/claims_test.go`):

| dropped leg | fixture | result |
|---|---|---|
| none | `start` by A at −2 h, both streams live | `Held{A, Since:-2h, LastActivity:-2h}` |
| 1 (closed) | both tickets closed | `Unclaimed` |
| 1 (archived) | sole open ticket archived | `Unclaimed` |
| 2 (no establishing verb) | `reopen`, `archive`, `close`, bare edit | `Unclaimed` |
| 3 (expired) | start −72 h, edit −48 h | `Unclaimed` |
| 4 (checkout gone) | start by A, live set = {B} | `Unclaimed` |

An unattributed latest establisher is no longer a fifth "dropped leg" row in this grid: it drops no leg at all. `TestUnattributedLatestStopsRatherThanScanning` above derives `Held{public}` with the earlier establisher contesting, and `TestColdStartDerivesThePublicCheckout` shows the all-unattributed case reads `Held` by the public checkout while fresh — the public checkout is a real holder, never a way of spelling "nobody".

---

## 7. The `internal/app` service layer — complete surface

`internal/app` contains exactly two non-test files: `app.go` (133 lines) and `claims.go` (62 lines).

### 7.1 `type App`

```go
type App struct {
	Workspace workspace.Info
	Store     storage.Store
	Stream    workspace.StreamID
}
```
`internal/app/app.go`. `Stream` documented as: always present under `AccessWrite` (minted on the checkout's first mutating command); present under `AccessRead` only if an earlier mutating command minted it, and its absence is the honest report that this checkout has produced no work evidence and therefore holds no claim (`internal/app/app.go`).

### 7.2 `AccessMode` / `accessContract` / `accessContracts`

Covered in §2.2. Type declarations at `internal/app/app.go`.

### 7.3 `Open(ctx, cwd, mode) (*App, error)`

`internal/app/app.go`. Ordered orchestration:
1. `contract, known := accessContracts[mode]`; unknown (including the zero value `""`) → `fmt.Errorf("invalid access mode %q", string(mode))`. The map lookup is both the validity check and the dispatch.
2. `workspace.Resolve(cwd)` → error returned as-is.
3. `engine.Open(ctx, contract.mode, ws.DatabasePath, ws.WorkspaceID)` → error returned as-is.
4. `contract.resolveStream(ws.PrivateGitDir)` — resolved **after** the store opens, so a command that cannot reach its store mints nothing.
5. On identity failure: `return nil, errors.Join(err, st.Close())` — the store must be closed because `Store.Close` also releases the workspace lock; joined so a stranded lock is visible too.
6. `st.AttributeTo(stream.Value())` — called unconditionally for both modes; only the value varies.
7. `return &App{Workspace: ws, Store: st, Stream: stream}, nil`.

Validation/behavior pinned by test:
- `AccessWrite` bootstraps a missing database; `AccessRead` fails with an error containing `"not initialized"` (`internal/app/app_test.go`).
- Read mode accepts the database write mode bootstrapped (`internal/app/app_test.go`).
- `""` and `"admin"` both fail with `"invalid access mode"` (`internal/app/app_test.go`).
- A damaged token file makes `Open` fail with a `"malformed"` diagnosis and the store must be released — proven by a repaired second open succeeding (`internal/app/app_test.go`).

No events are emitted by `app`; the package publishes no event/observer surface.

### 7.4 `OpenLocationForRead(ctx, loc) (storage.Store, error)`

`internal/app/app.go`. Opens a store at an already-derived `workspace.Location`, bypassing cwd git resolution entirely — the cross-project open primitive used by aggregation over many stores. Reads the foreign store's `workspace_id` from its own `config.json` via `workspace.ReadConfig(loc.ConfigPath)` — a pure read that never writes the foreign store, then `engine.Open(ctx, engine.ReadOnly, loc.DatabasePath, cfg.WorkspaceID)`. Always `ReadOnly`, so a foreign store gets the shared lock and never a second read-write engine the embedded Dolt driver would reject as "database is read only". **It mints no identity and calls no `AttributeTo`.**

### 7.5 `(*App).Close() error`

`internal/app/app.go`: `return a.Store.Close()`.

### 7.6 `(*App).LocalCheckouts() (claims.LocalCheckouts, error)`

`internal/app/claims.go`. Covered in §5.3. Placement rationale (effects at the boundary, `internal/claims` importing only `internal/model`) at `internal/app/claims.go`; workspace-id scoping as both a correctness and a privacy property.

### 7.7 `LiveCheckoutsOf(checkouts []workspace.Checkout) []claims.LiveCheckout`

`internal/app/claims.go`. Covered in §5.3. Exported because the CLI enumerates checkouts itself (it needs the addresses off the same listing) and projects them through this function rather than a copy of it (`internal/cli/claims_context.go`).

That is the entire `internal/app` surface: `App` (3 fields), `AccessMode` + 2 constants, `accessContract`, `accessContracts`, `Open`, `New`, `OpenLocationForRead`, `Close`, `LocalCheckouts`, `LiveCheckoutsOf`.

---

## 8. Gathering the claim context in the CLI

`type claimContext struct { standings claims.Standings; evidence claims.Evidence; self model.Attribution; addresses map[model.Attribution]workspace.Checkout }` (`internal/cli/claims_context.go`). `addresses` never reaches the shared database and lives only for the process's lifetime (`internal/cli/claims_context.go`).

`gatherClaimContext(ctx, stdout, ap)` (`internal/cli/claims_context.go`), in order:
1. `config.Load(pathspec.New(ap.Workspace.RootDir))`.
2. `ap.Store.ListIssues(ctx, storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})` — **both flags set**, because a lane's establishing event can sit on a deleted or archived issue; with the zero-value filter, a repository with even one deleted issue that ever carried an event made `NewEvidence` fail outright on every `next` and `backlog`.
3. `ap.Store.GetRelationsByIDs(ctx, ids)` → `parents[issue.ID] = relations[issue.ID].Parent`.
4. `ap.Store.ListAllEvents(ctx)`.
5. `claims.NewEvidence(allIssues, parents, events)`.
6. `workspace.LiveCheckouts(ap.Workspace.RootDir)`:
   - **On error**: prints to stdout, verbatim, `warning: could not enumerate local checkouts (%v) — claim liveness check and local addresses skipped, freshness alone governs\n`, and leaves `local` as the zero `claims.LocalCheckouts` and `addresses` nil. A failure to print the warning aborts the whole gather.
   - **On success**: `local = claims.NewLocalCheckouts(ap.Workspace.WorkspaceID, app.LiveCheckoutsOf(checkouts))` and `addresses = addressesByAttribution(ap.Workspace.WorkspaceID, checkouts)`.
7. `fresh := claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}`; `standings := claims.Derive(evidence, fresh, local)`.
8. `self := model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)` — a never-minted stream collapses to the zero Attribution, which is exactly "no live claims", with no branch needed.

The token projection is `app.LiveCheckoutsOf`, the same function `(*App).LocalCheckouts` uses (`internal/cli/claims_context.go`). `addressesByAttribution` indexes live checkouts by `model.NewAttribution(checkout.Stream.Value(), workspaceID)`, skipping tokenless checkouts (`internal/cli/claims_context.go`).

Callers: `next` (`internal/cli/next.go`), `workable`/`backlog` runner (`internal/cli/workable.go`), `authorizeStart` (`internal/cli/claims_takeover.go`), `reportContestedLanes` (`internal/cli/claims_contest_report.go`).

---

## 9. Gates: which operations consult claims, and what happens on conflict

### 9.1 `lit start` — the takeover gate (the only write gate)

`transitionSpec.authorize` is an optional hook that runs after the action is built and **before** `Store.Apply`, and may abort the transition by returning an error; only `start` supplies one, the other seven transitions use `noAuthorize` (`internal/cli/cli.go`). Wired at `internal/cli/cli.go`. The flag: `--take`, help string `"Confirm taking over a lane another checkout claims right now (required for non-interactive callers; an interactive terminal is prompted instead)"` (`internal/cli/cli.go`).

**`relationOf(standing, self) laneRelation`** — pure, no I/O (`internal/cli/claims_takeover.go`). It is the one place a `Standing` is read against an identity, and both the gate and routing consume its value, so they cannot disagree about whose lane it is.

| standing | condition | relation |
|---|---|---|
| `Held` | `self.Present() && held.By == self` | `laneOurs` |
| `Held` | otherwise | `laneHeldForeign` |
| anything else (`Unclaimed`, nil) | — | `laneUnclaimed` |

The `self.Present()` half is load-bearing, not a redundant guard: a checkout with no minted token has a zero `self`, and a zero `self` compared against a zero holder (the public checkout) would otherwise prove ownership of a lane this checkout never touched. So a lane the public checkout holds is always `laneHeldForeign` to every checkout, including one that has itself never minted a token.

There is no row for an expired claim. The derivation returns `Unclaimed` the moment the window closes (unless the holder's worktree is locked), so `relationOf` never learns a claim existed. The `laneRelation` constants are at `internal/cli/claims_takeover.go`. Pinned by `TestRelationOf` (`internal/cli/claims_takeover_test.go`).

**`authorizeStart(ctx, stdout, ap, issueID, prior, start, take) (notice string, err error)`** (`internal/cli/claims_takeover.go`):
1. `ap.Store.GetRelationsByIDs(ctx, []string{issueID})` → `lane := model.LaneOf(prior, relations[issueID].Parent)`.
2. `gatherClaimContext`.
3. Switch on `relationOf(cc.standings.Of(lane), cc.self)`: `laneHeldForeign` → `confirmFreshTakeover` runs and its error aborts the start; `laneUnclaimed` → returns `""` at once, no notice asked for.
4. Otherwise (the lane was held, ours or another's) returns `transferNotice(ctx, ap, issueID, start)`. The string is the line `transitionLeaf` writes after Apply (`transitionSpec.authorize`, `internal/cli/cli.go`); every other transition's hook (`noAuthorize`) returns `""`. An unclaimed lane, this checkout's own lane, and a lane whose claim has expired all pass with no output.

**`confirmFreshTakeover(stdout, cc, lane, take)`** (`internal/cli/claims_takeover.go`). It renders the claim line with `formatClaimLine(cc, lane, time.Now())`; `ok == false` → error `claims: %v is held by another checkout but has no claim line to show`, since the caller reaches here only for a `Held` standing.
- `take == true`, at a terminal or not → prints `"%s — taking over (--take)\n"` and proceeds. It is read before the terminal is.
- **Non-interactive** (`!isTerminal(stdout)`, the same signal `openOrPrintWorkflowFile` uses) and no `--take` → **refusal**: `takeoverUnconfirmedError{Message: fmt.Sprintf("%s — this lane is claimed and active; pass --take to confirm the takeover", line)}`.
- **Interactive** and no `--take`: prints `"%s\ntake over this lane? [y/N] "`, reads a line from `os.Stdin` via `bufio.NewReader(os.Stdin).ReadString('\n')`. A read error other than `io.EOF` → `fmt.Errorf("read takeover confirmation: %w", err)`. The answer is accepted iff `strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y")`; otherwise → `takeoverUnconfirmedError{Message: "takeover declined"}`.
- Both refusals are `takeoverUnconfirmedError`, which exits 3 with reason `takeover_unconfirmed`, whose remediation names `--take` (`exit.go`, `error_output.go`).

**`transferNotice(ctx, ap, issueID, start)`** (`internal/cli/claims_context.go`) returns `"claim transferred: %s -> %s\n"` when the ticket's recorded claimant (`claims.ClaimantOf`) was established and differs from the claimant the start installs; otherwise the empty string. Its one caller is `authorizeStart`, which asks only from a held lane, so a start on a lane nobody holds announces no transfer whatever the row's history records: an expired claim transfers nothing. Pinned by `TestTransferNoticeNamesAPredecessorThatMintedNoToken` (`internal/cli/claims_render_test.go`) and, for the expired lane, `TestStartOnAnExpiredForeignClaimIsSilent`.

E2E, over two real clones and a real git remote (`internal/cli/claims_takeover_e2e_test.go`): alpha starts and pushes; bravo's `start` without `--take` fails with an error containing both `--take` and `claimed`; the same command with `--take` prints `"taking over"` and the transfer line naming both assignees and both streams; and starting the now-bravo-held lane again produces neither `"claimed"` nor `"--take"` in the output. Expired path (`TestStartOnAnExpiredForeignClaimIsSilent`): with `freshness_window = "1ms"` and a 50 ms sleep, bravo's plain `start` succeeds and prints none of `claimed`, `stale`, `check for unmerged`, `take`, or `claim transferred`.

### 9.2 `lit next` — claim-aware routing (a read gate)

Registered `app.AccessRead`; it performs no writes (`internal/cli/register.go`). Flags (`internal/cli/next.go`, plus the hidden `--by` identity fallback): `--assignee`, `--type`, `--status` (`open|in_progress`), `--labels`, and `--all`, help string `"Ignore the focus scope and route over the whole queue"`. No `--limit`, no `--columns`. `--continue` is retired: `--continue` and `--continue=<x>` are wrapped at the parse boundary as an `UnsupportedError` carrying ``--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag`` (`internal/cli/flagset.go`).

The leaf gathers rows, relation details and the focus scope, then the claim context, then routes: `routeNext(rows, details, cc.standings, cc.self, focus.scopeFor(*all))` (`internal/cli/next.go`). `focusScope.scopeFor(all)` returns the zero `focusScope` — the whole queue — when `--all` is set, so the flag picks a value and every stage after it stays unconditional (`internal/cli/ready_state.go`).

**`NextOutcome`** is a sealed sum (`internal/cli/next_route.go`, markers) — **eight** cases:
- `ServedFromClaim{Row}` — a ready ticket in a lane this checkout already holds. Routing step 1; no new claim is established, so nothing is announced.
- `ResumedOwnWork{Row}` — a ticket already in flight in a lane this checkout holds, handed back to its holder. Routing step 1 for work that is started rather than startable; nothing is claimed and nothing is begun, so it reports a state rather than an act.
- `ServedFromEpicLane{Row, Lane model.LaneID}` — a pick from a different lane of the same epic this checkout already holds a lane in. Routing step 2. Two fields exactly: the epic is `Lane.Epic()`, and carrying it beside the lane would be two clocks for one fact.
- `ServedFromNewLane{Row, Lane model.LaneID}` — a ready ticket in a lane this checkout does **not** hold. **One** step produces it: the global pool (step 4). Step 1b once shared it and now has its own `ServedFromDependency`, because sharing this type left the renderer unable to tell the two picks apart (`links-next-output-4hor`). `Lane` is the `model.LaneID`, not its `String()`: stringifying here would throw away the discriminator the renderer needs to tell a lane worth naming from one that would only repeat the ticket.
- `ServedFromDependency{Row, Lane model.LaneID, Gates string}` — a ready ticket outside the lanes this checkout holds that gates one of this checkout's blocked rows. **Two** steps produce it: routing step 1b, for a row in a lane this checkout holds, and step 2b, for a row anywhere in its epic. Starting it would establish a claim on a lane this checkout does not hold, as with `ServedFromNewLane`, which is why `Lane` is carried. `Gates` is the id of the blocked row the pick unblocks — open, in a lane this checkout holds (1b) or in its epic (2b) — and is always set by construction: a dependency is only yielded because some in-scope row depends on it.
- `ServedPastExhaustion{Row, Lane model.LaneID, Exhaustion Exhausted}` — the checkout's own claimed epic(s) have open work with none of it reachable, and the global pool has a ready ticket outside them. Routing step 3. It carries the whole exhaustion because the announcement names why the epic stopped and, when something blocks it, the route that stays in it (links-next-5sxz).
- `Exhausted{Epics []string, Blocked []rowReach, Held []rowReach, OffPath []rowReach}` — the same, with nothing ready in the global pool either. Routing step 3. `Held` is `heldByThemselves` over the rows in `ourScope`: the open rows, in rank order, that `IssueReadiness.HeldByItself()` reports held by a reason about the row itself (a reserved label, a missing field) rather than by a dependency or an earlier sibling, each classified by `reachOf`. `OffPath` is the rows outside the scope that a focus label withheld from the pool, empty with no focus active and on `ServedPastExhaustion`.
- `NoWork{Unreachable []rowReach}` — the global pool handed back nothing.

`Exhausted` and `NoWork` are themselves `error` implementations and travel outward **as themselves** rather than being rendered into a generic error, which is what keeps the exit-code and reason sinks reading the routing verdict instead of a copy that could drift (`internal/cli/next.go`).

**`capacityFor(row, standing, self) capacity`** — the admission rule; pure, total, no I/O (`internal/cli/next_route.go`). Three capacities: `routeAround` (not this checkout's to take right now), `serveWork`, `resumeWork` (`internal/cli/next_route.go`). It reads `relation := relationOf(standing, self)` and `started := row.State() == model.StateInProgress`, and asks `ClassifyReadiness(row.Annotations).IsReady()` only of a row not yet started. Evaluated top-down:

| # | condition | capacity |
|---|---|---|
| 1 | `relation == laneHeldForeign` | `routeAround` |
| 2 | `!started` and ready | `serveWork` |
| 3 | `!started`, otherwise | `routeAround` |
| 4 | `started` and `relation == laneOurs` | `resumeWork` |
| 5 | `started`, otherwise (`laneUnclaimed`) | `serveWork` |

Row 5 is where abandonment lives: an in-flight row in a lane nobody holds is abandoned by definition, because whoever started it no longer holds a claim there, and it is served on that fact alone. The orphan annotation — the row's own quiet clock — does not enter routing; `lit backlog` and `lit orphaned` still read it as a description of the row. `laneHeldForeign` — a lane another checkout holds — is the one relation routed around on ownership alone; a locked worktree past the clock derives `Held`, so it is one of these.

**`ownScope(standings, self) (map[model.LaneID]bool, map[string]bool)`** (`internal/cli/next_route.go`): every lane whose `relationOf(standing, self)` is `laneOurs`, plus each such lane's non-empty `Epic()`. It reads the **standings**, not the gathered rows — the rows are already narrowed by `--type/--labels/--assignee`, and deriving ownership from them let a display filter empty the set and drop the whole self-aware branch.

**`routeNext(rows, details, epics, standings, self, scope focusScope) NextOutcome`** — six parameters (`internal/cli/next_route.go`); `epics` is the relations of the epics above the rows, keyed by epic id, and is read only when the checkout holds a lane, where steps 1b and 3 use `workToward := workTowardEach(rows, details, epics)`. Closures: `laneOf`, `verdict` (= `capacityFor`), `reachFor` (= `reachOf`), and `pickFrom(from, inScope, accept ...capacity)`, which keeps the first row of `from`, in the gather's composite-rank order, that sits in an admitted lane and carries one of the accepted verdicts. `accept` is a **set**, never a preference order: composite rank is the only tiebreak routing applies, and ranking capacities against each other would pass over the backlog's #1 row, abandoned in flight, for a lower-ranked leaf that was merely ready.

Precedence, with `ownLanes, ownEpics := ownScope(standings, self)` and `mine := func(lane) bool { return ownLanes[lane] }`. If `len(ownLanes) > 0`:
1. **Step 1** — our own lanes, accepting `{serveWork, resumeWork}`, whichever the backlog ranks first. `resumeWork` → `ResumedOwnWork{Row}`; `serveWork` → `ServedFromClaim{Row}`.
2. **Step 1b** — `onPathDependency(gatingDependencies(rows, laneOf, mine, reachFor, workToward), laneOf)`, the work toward a dependency outside our lanes that gates one of them → `ServedFromDependency{Row: dep.Row, Lane: laneOf(dep.Row), Gates: dep.Gates, Blocker: dep.ID}`. `Row` is the dependency itself when it is a leaf, or a ticket under it when it is an epic. It establishes a claim on a lane we do not hold, so it is announced as one; `Gates` carries the blocked row it unblocks and `Blocker` the dependency, so the line can say what the pick is for .
3. **Step 2** — the rest of our epic, in lanes we do not already hold: predicate `lane.Epic() != "" && ownEpics[lane.Epic()] && !mine(lane)`, accepting `serveWork` → `ServedFromEpicLane{Row, Lane}`.
4. **Step 2b** — `gating := gatingDependencies(rows, laneOf, ourScope, reachFor, workToward)`, where `ourScope` is `mine(lane) || ourEpic(lane)`; `onPathDependency(gating, laneOf)` → `ServedFromDependency`, as in step 1b. A dependency that directly gates any of our epic outranks every ticket the pool could offer.
5. **Step 3** — `exhausted := Exhausted{Epics: slices.Sorted(maps.Keys(ownEpics)), Blocked: blockedRows(gating), Held: heldByThemselves(rows, <ourScope of the row's lane>, reachFor)}` — the same walk step 2b declined. If the pool pick (below) finds a row → `ServedPastExhaustion{Row, Lane: laneOf(row), Exhaustion: exhausted}`; otherwise `exhausted.OffPath` is set to `withheldByScope(<offPath minus rows whose lane ourScope admits>)` and `exhausted` is returned. The pick is outside our epic by construction: steps 1 and 2 took every `serveWork` row in our lanes and our epic, and the pool is a subset of the rows they walked.

**Step 4** is reached directly by a checkout holding no lanes: `pool, offPath := scope.partition(rows)`, then `pickFrom(pool, <every lane>, serveWork)` — the same pick step 3 falls through to — → `ServedFromNewLane{Row, Lane}`, else `NoWork{Unreachable: append(passedOver(pool, reachFor), withheldByScope(offPath)...)}`. The scope-withheld rows travel into the diagnostic rather than vanishing, because "nothing is startable" and "nothing on your focus path is startable" are different answers . `withheldByScope` stamps them `reachOffFocusPath` at the one place that applied the scope ; `passedOver` classifies every row the pool walk went through, all `routeAround` by construction .

Steps 1-3 walk every gathered row; step 4 walks the focus-scoped pool. The row set is passed to `pickFrom` explicitly at each call so that difference stays visible. The focus scope narrows step 4 and nothing else.

**`reachKind`** (`internal/cli/next_route.go`) is what one row is to this checkout right now — five classifications plus the pool walk's own, and a bound: `reachTakeable`, `reachHeldFresh`, `reachNotReady`, `reachAwaitingOutside`, `reachOutOfView`, `reachOffFocusPath`, `reachKindCount`. A bool here read "takeable or not", so a row outside the run's filtered view, or one not startable itself, rendered as the one reason the message named: claimed by another checkout. `rowReach{ID string; Row annotation.AnnotatedIssue; Kind reachKind}`.

**`reachOf(row, standing, self)`**: `capacityFor(...) != routeAround` → `reachTakeable`; `relationOf(...) == laneHeldForeign` → `reachHeldFresh`; `ClassifyReadiness(row.Annotations).AwaitsOutside()` → `reachAwaitingOutside`; otherwise `reachNotReady`. Exhaustion asks it of the dependencies gating our scope; an empty global pool asks it of every row the walk went past.

**`workTowardEach(rows, details, epics)`**: the gathered rows, in rank order, indexed by every dependency finishing them is work toward — each row under its own id, and under every epic `epicsAbove` yields for it. **`gatingDependencies`**: the distinct open dependency ids, in rank order, gating the open rows whose lane `inScope` admits, each already carrying its `reachKind` and the id of the in-scope row it gates — the yielded `gatedDep` embeds `rowReach` and adds `Gates`. A dependency's `Kind` is the lowest `reachKind` among `workToward[id]`, and its `Row` the first row in rank order with that kind; with no row there it is `reachOutOfView` with a zero `Row`. **`onPathDependency(deps []gatedDep, laneOf) (ServedFromDependency, bool)`** returns the first of those whose `Kind == reachTakeable`, served as its `Row`. A same-lane gate never reaches here: it shares the blocked row's lane, so step 1 or 2 already served it.

**`describeReach(rows, lead, notes)`** renders `"<lead><names> (<note>)"` for each kind that has rows, joined by `"; "`, in `reachKind`'s declaration order. An entry's name (`rowReach.name`) is its `ID`, or `"<Row.ID> under <ID>"` when it carries a `Row` whose id differs — a dependency read through a ticket under it. `nameIDs` names at most `maxNamedPerKind = 12` ids and states how many it left out. `reachNotes` is `[reachKindCount]string`, indexed by the kind itself.

`exhaustedNotes`, exact strings (no `reachTakeable`: exhaustion reports the very walk step 2b declined, so it never holds one):
- `reachHeldFresh`: `on your path but claimed by another checkout right now`
- `reachNotReady`: ``on your path but not startable right now — `lit show` it``
- `reachAwaitingOutside`: ``on your path but waiting on an event outside this repository — `lit show` it names the event``
- `reachOutOfView`: ``on your path but outside this view — `lit show` it``

`poolNotes`, exact strings:
- `reachHeldFresh`: `in progress or claimed in a lane another checkout holds right now`
- `reachNotReady`: ``not startable right now — `lit show` it names what blocks it``
- `reachAwaitingOutside`: ``waiting on an event outside this repository — `lit show` it names the event``
- `reachOffFocusPath`: ``off the focus path this run answered over — `lit next --all` to route over the whole queue``

**`Exhausted.scope()`**: `fmt.Sprintf("epic(s) %s", strings.Join(o.Epics, ", "))` when `Epics` is non-empty, else `"your claimed lane(s)"`.

**`Exhausted.home()`**: ``"with `lit new --top`"`` when `Epics` is empty; otherwise `"under the epic with "` + one ``fmt.Sprintf("`lit new --parent %s --top`", epic)`` per epic, joined by `" or "`.

**`Exhausted.stay()`** is `nil` when every entry of `Blocked` is `reachAwaitingOutside`, which includes `Blocked` empty; otherwise the one route ``"to stay, file the ticket that clears a blocker " + home() + ", then make that blocker wait on it with `lit dep add --from <new> --to <blocker>`"``.

**`Exhausted.why()`**:
- `no ready work in %s — %s`, the second being the non-empty clauses of `describeReach(o.Blocked, "blocked on ", exhaustedNotes)` and `describeReach(o.Held, "held here: ", exhaustedNotes)`, joined by `"; "`.
- with both empty, the second is `nothing else is queued behind what's already in progress`.

**`Exhausted.outside()`**:
- `len(o.OffPath) == 0`: ``"`lit next` has nothing ready outside it either"``
- otherwise: `"nothing on the focus path outside it is ready either: "` + `describeReach(o.OffPath, "", poolNotes)`.

**`Exhausted.Error()`**: `why() + "; " + outside()`, then each `stay()` route, joined by `" — "`.

**`NoWork.Error()`**:
- `len(o.Unreachable) == 0` → `no ready work`.
- `o.withheld()` — any row of kind `reachOffFocusPath` → `no ready work on the focus path — the backlog is not empty, and each row below says why this run did not serve it: %s`.
- otherwise → `no ready work — the backlog is not empty, but nothing in it is startable here: %s`.

The `%s` in both non-empty arms is `describeReach(o.Unreachable, "", poolNotes)`.

**Exit code and reason.** `ExitNoWork = 6` (`internal/cli/exit.go`). **Both** `Exhausted` and `NoWork` map to it (`internal/cli/exit.go`): distinct from `ExitGeneric` because a caller looping `lit next` has to tell "stop, there is nothing for you" from "lit is broken", and under one code its only way to do that was to parse the English; not `ExitOK`, because for `lit next` 0 means "a ticket is on stdout", and exiting 0 with no row would hand the caller a success-shaped void (`internal/cli/exit.go`). Reasons: `scope_exhausted` for `Exhausted`, `no_ready_work` for `NoWork` (`internal/cli/error_output.go`).

**`startAdvice(row, lane)`** (`internal/cli/next.go`) — the line every pick that would establish a claim prints above its row: what running `lit start` would lock, never what this command did. `lit next` claims nothing and starts nothing. `object, named := lane.Describe()`, with `object = "it"` when the lane is not named; the advice is ``run `lit start %s` to claim %s`` (Row.ID, object), and an in-progress row prefixes it with ``%s is in progress and nobody holds it — `` (Row.ID). Routing serves an in-progress row from a lane this checkout does not hold only when nobody holds that lane, so the prefix states exactly what the standing proves and nothing about who left the row or when — an expired claim is not a claim, and the row's history is `lit show`'s to tell.

**`LaneID.Describe() (string, bool)`** (`internal/model/model.go`) — three cases:
- solo lane → `("", false)`. A solo lane is the ticket that names it, so any phrase for it only repeats what the surrounding sentence already said.
- empty key → `(fmt.Sprintf("the default lane of epic %s", l.epic), true)`.
- otherwise → `(fmt.Sprintf("lane %s of epic %s", l.key, l.epic), true)`.

**`renderNextOutcome(w, outcome, details, cc, actingAs)`** (`internal/cli/next.go`):
- `ServedFromClaim` → no announcement at all.
- `ResumedOwnWork` → `resumeAdvice(o.Row, cc.actingAs)` + `"\n"`. Two sentences, chosen by
  whether the row carries an assignee that is not the identity running the command
  (`internal/cli/next.go`). Both halves must be non-empty and differ, so an
  unassigned ticket and a command with no session identity both take the lane's own
  sentence: `"%s is already in progress in a lane you hold — continue where you left off"`
  (Row.ID). Otherwise: ``%s is in progress and assigned to %s, not to you — check that they have stopped before you continue it, or take other work from `lit backlog` ``
  (Row.ID, assignee). Lanes are keyed on the checkout, not the session, so two sessions
  in one checkout share every lane, and the assignee is the only fact that separates them.
  Nothing decides whether the named holder is still running — every session mints a new
  identity, so a predecessor and a live peer both read as "not you", and lit carries no
  liveness probe — so the line asks for the check instead of adjudicating. The sentence
  claims no more than the mismatch proves: an assignee is free text and need not name a
  session at all (links-routing-t6fa).
- `ServedFromEpicLane` → `startAdvice(o.Row, o.Lane)` + `" (a second lane of an epic you already hold a lane in)\n"`.
- `ServedFromNewLane` → `startAdvice(o.Row, o.Lane)` + `"\n"`.
- `ServedFromDependency` → the same `startAdvice(...)` + `dependencyReason(o)` + `"\n"`: `" (gates %s, which is on your path)"` on `Gates` when `Blocker == Row.ID`, else `" (it is in epic %s, which gates %s on your path)"` on `Blocker`, `Gates`.
- `ServedPastExhaustion` → `o.Exhaustion.why()` + `"\n"`, then the routes `o.Exhaustion.stay()` followed by `"move on to the top ready ticket outside it: " + startAdvice(o.Row, o.Lane)`, joined by `"\nor "`, then `"\n"`.
- `Exhausted`, `NoWork` → returned as themselves; nothing printed.
- default → `panic(fmt.Sprintf("renderNextOutcome: unhandled NextOutcome %T", outcome))`.

For the five served cases the announcement is written only when non-empty, then `lane := model.LaneOf(row.Issue, details[row.ID].Parent)` and `printNextSummary(w, row, cc, lane)` (`internal/cli/ready_state.go`); finally `nextPulledOccasion(row.Issue)` is returned and dispatched to workflows (`internal/cli/next.go`).

### 9.3 `lit sync reconcile` — the contest report (a read gate on merge)

`reportContestedLanes(ctx, stdout, ws, syncStore)` (`internal/cli/claims_contest_report.go`):
- Builds a temporary `&app.App{Workspace: ws, Store: syncStore}` — note: **no `Stream`**, so `cc.self` is the zero Attribution for this call (cf. `internal/cli/claims_context.go`).
- `lanes := contestedLanes(cc.standings)`; if empty, returns nil silently.
- Header, verbatim: `contested: evidence from more than one checkout just met for these lanes —`.
- Per lane: `"  %s: %s\n"` with `lane` (via `LaneID.String()`) and `formatClaimLine(cc, lane, now)` where `now = time.Now()` taken once for the whole report.
- `ok == false` from `formatClaimLine` → error `contested lane %s reported no claim line — standings and rendering disagree`.

`contestedLanes(standings)` (`internal/cli/claims_contest_report.go`): every lane whose standing is `claims.Held` with `len(held.Contested) > 0`; returns a non-nil empty slice otherwise; sorted by `strings.Compare(a.String(), b.String())`. Pinned by `TestContestedLanesFiltersAndSorts` (Unclaimed, Stale, and uncontested Held all drop out; survivors sorted) (`internal/cli/claims_contest_report_test.go`) and `TestContestedLanesEmptyForNoContest`.

**Call sites**: only two — after `storage.SyncReconcileLinearized` (`internal/cli/sync_reconcile_cmd.go`) and after `storage.SyncReconcileCombined` (`internal/cli/sync_reconcile_cmd.go`), the two states where histories actually merged (`internal/cli/sync_reconcile_cmd.go`). Not called for prose-pending, unrelated-histories, or not-diverged outcomes.

E2E: two clones partition-start the same lane; `lit sync reconcile` on bravo prints output containing `"contested"` and the ticket id (`internal/cli/claims_contest_report_e2e_test.go`). Negative half: an ordinary reconcile with no shared lane never mentions `"contested"`.

### 9.4 Surfaces that render but do not gate

- `lit backlog` — `printBacklogContext` prints the claim line, indented, after the `in_progress:` line and before `unblocks:` (`internal/cli/backlog.go`). `backlogView` is the only `workableView` preset (`internal/cli/workable.go`), and its render function is `printBacklogOutput(w, columns, issues, details, cc)` (`internal/cli/backlog.go`).
- `printInlineDeps` — the shared epic/depends-on/claim/unblocks block used by `lit next`'s summary, printing the claim line between `depends on` and `unblocks` (`internal/cli/ready_state.go`). `printNextSummary` calls it after the issue's column line (`internal/cli/ready_state.go`).

No other command consults `claims.Standings`: the only readers of `cc.standings` / `cc.self` outside `internal/cli/claims_*.go` are `next.go` (routing) — everything else consumes `cc` only for rendering (`internal/cli/workable.go`, `internal/cli/backlog.go`, `internal/cli/ready_state.go`).

---

## 10. Rendering

### 10.1 `formatClaimLine(cc, lane, now) (string, bool)`

`internal/cli/claims_render.go`. Returns `("", false)` for anything that is not `Held` — an Unclaimed lane renders **no line at all**, not an empty or placeholder one; and a lane whose claim has expired is Unclaimed, so nothing is printed about it even when its holder's worktree is still on this machine and resolvable through `cc.addresses` (pinned `TestFormatClaimLineUnclaimedLaneRendersNothing`, `TestFormatClaimLineExpiredClaimRendersNothingEvenWithAnAddress`, `internal/cli/claims_render_test.go`).

- `Held` → `line = claimPrefix(held.By, cc)`; if `len(held.Contested) > 0`, append `fmt.Sprintf(" · contested by %s", strings.Join(nameCheckouts(held.Contested), ", "))`.
- Then `parts := []string{line, humanizeCoarseDuration(now.Sub(held.LastActivity)) + " ago"}`; if `formatLaneProgress(cc.evidence.LaneProgress(lane))` is non-empty, append it; join with `" · "`.

**Two tiers**: the dossier (holder badge, freshness, lane progress) comes entirely from `cc.evidence` and `cc.standings` — the shared, synced data — so it renders identically on any clone; the address renders only when `cc.addresses` resolves the holder to a live worktree **this machine** enumerated (`internal/cli/claims_render.go`).

### 10.2 `claimPrefix(by, cc)`

`claimPrefix` (`internal/cli/claims_render.go`):
- If `cc.addresses[by]` resolves: `branch := checkout.Branch`; if empty, `branch = "detached HEAD"`; returns `fmt.Sprintf("claimed here: %s (%s)", checkout.Path, branch)`.
- Otherwise: returns `fmt.Sprintf("claimed: %s (%s)", nameCheckout(by), holdState(by))`.
- `holdState(by)`: `"elsewhere"` (an identified `by`) or `"unaddressed"` — the public checkout, since `by` is the zero Attribution and `by.Present()` is false.
- The prefix carries no freshness tag: a locked worktree past the clock derives an ordinary `Held` lane and renders like any other, and an expired claim derives `Unclaimed` and renders nothing.
- **The public checkout never renders `claimed here`, whoever is asking.** `by` is the zero Attribution and a live worktree's holder in `cc.addresses` is always an identified checkout, so the first branch above can never match it — an earlier version compared `by` against `cc.self` instead and misread that coincidence as proof of ownership, rendering `"claimed here: this checkout"` for a foreign lane on `lit sync`'s contested-lane report (whose `cc.self` is always the zero Attribution). Pinned by `TestFormatClaimLinePublicCheckoutIsNeverHere` (`internal/cli/claims_render_test.go`): `"claimed: the public checkout (unaddressed)"` in every row regardless of `cc.self`.

### 10.3 `formatLaneProgress(progress)`

`internal/cli/claims_render.go`:
- `Total == 0` → `""`.
- `Active != nil` → `fmt.Sprintf("%s in progress, %d/%d done", progress.Active.ID, progress.Done, progress.Total)`.
- else → `fmt.Sprintf("%d/%d done", progress.Done, progress.Total)`.

### 10.4 `nameCheckout` / `nameCheckouts`

`internal/cli/claims_render.go`: an identified checkout is named `"stream " + <token, truncated to 8 chars>` (`labelLen = 8`) — a display nicety, not a privacy measure, since the full token is already opaque. The zero Attribution — the public checkout — is named the literal `"the public checkout"` instead of being routed through the token label: reading its empty stream through that path produced `"stream "` with nothing after it, an answer-shaped void. `nameCheckouts` maps a slice through `nameCheckout`, used for the contest suffix.

### 10.5 `humanizeCoarseDuration`

`internal/cli/output.go`, buckets:
- `>= 48h` → `"%d days"` (`int(d/(24*time.Hour))`)
- `>= 2h` → `"%d hours"` (`int(d/time.Hour)`)
- `>= 2m` → `"%d minutes"` (`int(d/time.Minute)`)
- else → `"under a minute"`

### 10.6 Rendering behavior pinned by test

- Unclaimed renders no line at all (`TestFormatClaimLineUnclaimedLaneRendersNothing`).
- Dossier without any local address: line says `"elsewhere"`, carries `"1/2 done"`, names `"active-ticket in progress"`, and reads `"2 hours ago"` (`TestFormatClaimLineDossierNeedsNoLocalAddress`).
- With an address entry: `"claimed here: ../links-wt-pgct (links-claims-1ihf.11)"`; the same standing rendered without addresses must not carry the path and must say `"elsewhere"` (`TestFormatClaimLineAddressOnlyOnClaimantsOwnMachine`).
- An expired claim whose holder's worktree is still resolvable renders nothing at all (`TestFormatClaimLineExpiredClaimRendersNothingEvenWithAnAddress`).
- The public checkout, whatever `cc.self` is asking: `"claimed: the public checkout (unaddressed)"`, never `"claimed here"` (`TestFormatClaimLinePublicCheckoutIsNeverHere`).
- Contested Held: line contains `"contested by " + nameCheckout(contestant)` (`TestFormatClaimLineContestedAppendsContestants`).

---

## 11. Privacy invariants stated in code

- Both halves of `Attribution` are opaque by mandate; nothing user-, host-, or path-shaped may travel there, because the database syncs to shared remotes; resolving a token to a physical checkout happens only on the machine that owns it (`internal/model/model.go`).
- `StreamID` is deliberately meaningless — no directory name, hostname, or username material (`internal/workspace/stream.go`).
- `--by`'s old `os.Getenv("USER")` default was removed as a documented-invariant violation; the fallback is `""` → the opaque `"unknown"` (`internal/cli/cli.go`).
- `Checkout.Path` / `Checkout.Branch` stay on the local machine (`internal/workspace/checkouts.go`); `claimContext.addresses` never reaches the shared database and lives only for the process (`internal/cli/claims_context.go`).
- A different clone of the same repository on the same machine carries a different workspace id, so this machine's enumeration never speaks to its claims (`internal/app/claims.go`).

---

## 12. End-to-end acceptance already proven in tests

`TestDeletedCheckoutReleasesItsClaimHereAndAgesOutElsewhere` (`internal/app/claims_test.go`), driven against real git worktrees and the real store:
1. A linked worktree opens `AccessWrite` (minting its token), creates an issue, and applies `model.Start{Assignee: "worker"}`; `lane = model.LaneOf(issue, nil)`.
2. The primary reads: `Derive(evidence, {Now: time.Now(), Window: 24h}, local).Of(lane)` is `Held` with `held.By.Stream() == workerToken`.
3. `git worktree remove --force`.
4. The primary's **very next** derivation over a freshly re-read evidence set reports `Unclaimed` — no waiting, no window lapse, no cleanup step.
5. A second clone (`workspaceID + "-a-different-clone"`, unrelated live tokens) derives the **same** evidence and still reports `Held` by the worker — it must age the claim out like any remote.

`fresh()` in that suite is `claims.Freshness{Now: time.Now(), Window: 24 * time.Hour}` (`internal/app/claims_test.go`); the read path used is `ListIssues{IncludeArchived:true, IncludeDeleted:true}` + `GetRelationsByIDs` + `ListAllEvents` + `NewEvidence` (`internal/app/claims_test.go`).
