# Behavioral inventory — claims / assignment subsystem

All paths relative to `/Users/bmf/code/links-issue-tracker`. Every claim below carries a `file:line` citation. Derived from Go source only.

---

## 1. What a claim IS

### 1.1 Nothing is stored

A claim is a *read-time derivation* over records the database already holds. `internal/claims/evidence.go:1-15` states the package "derives, at read time, which checkout is working which lane. Nothing here is stored." The package imports only `cmp`, `fmt`, `slices`, `time`, `strings`, and `internal/model` (`internal/claims/evidence.go:17-23`, `internal/claims/derive.go:3-9`); it reads no clock, no store, no filesystem — the clock reading arrives as data (`internal/claims/derive.go:20-23`).

There is no claims table, no claim row, no claim file. The only persisted footprint is `model.Attribution` stamped on each `model.IssueEvent` (`internal/model/model.go:612-616`, `internal/model/model.go:722-733`).

### 1.2 The persisted primitive: `model.Attribution`

```go
type Attribution struct {
	stream    string
	workspace string
}
```
`internal/model/model.go:631-634`.

- Both halves unexported; `NewAttribution` is the only constructor (`internal/model/model.go:656-661`).
- `NewAttribution(stream, workspace)` returns the zero value if **either** half is empty — "complete pair or nothing" (`internal/model/model.go:657-659`). The collapse is silent by design (`internal/model/model.go:642-647`).
- Accessors: `Stream()`, `Workspace()` (`internal/model/model.go:666-667`); `IsZero()` = `a == Attribution{}` (`internal/model/model.go:672`); `Present()` = `!IsZero()` (`internal/model/model.go:682`).
- Wire form is a separate struct `attributionWire{Stream string \`json:"stream,omitempty"\`; Workspace string \`json:"workspace,omitempty"\`}` (`internal/model/model.go:686-690`), marshalled at `internal/model/model.go:692-694`.
- `UnmarshalJSON` routes through `NewAttribution`, so `{"stream":"x"}` with no workspace decodes to the absent pair (`internal/model/model.go:706-712`).
- Absence is a permanent legal state: attribution is never backfilled onto events that predate the feature (`internal/model/model.go:674-680`, `internal/model/model.go:722-723`).
- Field on the event: `Attribution Attribution \`json:"attribution,omitzero"\`` (`internal/model/model.go:731`) — `omitzero` consults `IsZero`, so an unattributed event writes no attribution object at all (`internal/model/model.go:669-671`).

### 1.3 The derived value: `Standing`

Sealed sum with three variants, discriminated by unexported marker `isStanding()` (`internal/claims/standing.go:17`, `internal/claims/standing.go:62-64`):

- `Unclaimed struct{}` — no holder, no provenance (`internal/claims/standing.go:24`).
- `Held struct { Tenure; Contested []model.Attribution }` (`internal/claims/standing.go:47-50`).
- `Stale struct { Tenure }` (`internal/claims/standing.go:58-60`).

`Tenure`:
```go
type Tenure struct {
	By           model.Attribution
	Since        time.Time
	LastActivity time.Time
}
```
`internal/claims/standing.go:34-38`. `Since` = timestamp of the establishing event that put the holder there; `LastActivity` = the holder's most recent mutation of any kind in the lane, and freshness is measured against `LastActivity`, not `Since` (`internal/claims/standing.go:28-33`).

`Standings map[model.LaneID]Standing` with total read `Of(lane)`: a missing key returns `Unclaimed{}` rather than a nil interface (`internal/claims/standing.go:71-78`).

### 1.4 The unit a claim is held over: `model.LaneID`

```go
type LaneID struct { epic string; key string; solo bool }
```
`internal/model/model.go:188-192`. Fields unexported; `LaneOf` is the only constructor (`internal/model/model.go:184-187`).

`LaneOf(issue, parent)` (`internal/model/model.go:212-217`):
- `parent == nil` **or** `!parent.IsContainer()` → `LaneID{key: issue.ID, solo: true}` (a "lane of one").
- otherwise → `LaneID{epic: parent.ID, key: issue.Lane}`. An epic that declares no lanes is exactly one lane keyed by `""` (`internal/model/model.go:206-209`).

`Epic()` / `Key()` accessors at `internal/model/model.go:221-222`. `String()`: solo → bare key; otherwise `epic + "#" + key`, so an epic's unnamed default lane renders `"E#"` (`internal/model/model.go:228-233`).

Test coverage of the three shapes: epic-major lanes `TestLaneGranularity` (`internal/claims/claims_test.go:321-332`), cross-epic `TestCrossEpicDependencyClaimsOnlyItsOwnLane` (`internal/claims/claims_test.go:337-356`), solo `TestParentlessTicketIsItsOwnLane` (`internal/claims/claims_test.go:359-368`).

---

## 2. Identity derivation

There are **two distinct identity systems**, and they do not share a value:

| system | value | where it lives | who reads it |
|---|---|---|---|
| checkout identity (claims) | `workspace.StreamID` + workspace id | `<private git dir>/lit-stream` (local only) | claim derivation |
| acting identity (assignee/actor strings) | `claude_<session>` / flag value | flags + `CLAUDE_CODE_SESSION_ID` | `Issue.Assignee`, `IssueEvent.Actor`, `CreatedBy` |

### 2.1 Checkout identity — `StreamID`

- `type StreamID struct{ value string }`, unexported field so no arbitrary string can become one (`internal/workspace/stream.go:54`). `Value()` (`:60`), `Present()` = `value != ""` (`:66`).
- Zero value means "this checkout has never minted a token" and is a legitimate state, not an error (`internal/workspace/stream.go:51-53`).
- Storage file name: `lit-stream` (`internal/workspace/stream.go:22`), stored in the checkout's **private** git dir (`--git-dir`), never the common dir — so every worktree of a repo shares one backlog but carries a distinct token, and `git worktree remove` deletes the token with the directory (`internal/workspace/stream.go:13-21`).
- Entropy: 8 bytes from `crypto/rand` (`internal/workspace/stream.go:28`, `internal/workspace/stream.go:232-238`), unpadded base32 (`internal/workspace/stream.go:38`), lowercased → alphabet `[a-z2-7]`, length `(8*8+4)/5 = 13` characters (`internal/workspace/stream.go:30-33`).
- File content is the token plus a trailing newline (`internal/workspace/stream.go:175-178`); file mode set explicitly to `0644` (`internal/workspace/stream.go:182-187`).

**`ReadStream(privateGitDir)`** (`internal/workspace/stream.go:79-89`):
- Missing file → zero `StreamID`, nil error.
- Any other read error → wrapped error `read stream id %q: %w`.
- Present file → `parseStreamToken`.

**`parseStreamToken`** (`internal/workspace/stream.go:260-275`): trims whitespace; rejects anything not exactly 13 chars ("expected %d characters, found %d") and any character outside `a-z`/`2-7` ("character %q is outside the token alphabet"). Error text always appends the remedy: `delete the file to mint a fresh identity for this checkout (work already recorded under the old identity keeps it, and any lane this checkout held is released)` (`internal/workspace/stream.go:280-282`). Never self-heals (`internal/workspace/stream.go:255-259`).

**`EnsureStream(privateGitDir)`** (`internal/workspace/stream.go:106-130`): read-first fast path; if absent, `publishStreamToken`, then re-read; if still absent after publishing → error `stream id %q vanished immediately after it was written` (`:126-127`). The FILE, never the freshly-minted candidate, decides identity (`internal/workspace/stream.go:102-105`).

**`publishStreamToken`** (`internal/workspace/stream.go:150-227`): mints token → `os.CreateTemp` in the same directory → write + `Chmod(0644)` + `Sync` + `Close` → `os.Link(temp, final)`. `os.ErrExist` from the link is a **success** (a racing caller already published) (`internal/workspace/stream.go:203-205`). Any other link failure produces an error naming the hard-link requirement explicitly (`internal/workspace/stream.go:224`). Temp file removed via `defer` on all paths (`internal/workspace/stream.go:173`). The directory entry is deliberately not fsynced (`internal/workspace/stream.go:188-192`).

### 2.2 Which commands mint

`app.AccessMode` is `"read"` or `"write"` (`internal/app/app.go:30-35`). The mapping table:

```go
var accessContracts = map[AccessMode]accessContract{
	AccessRead:  {mode: engine.ReadOnly, resolveStream: workspace.ReadStream},
	AccessWrite: {mode: engine.ReadWrite, resolveStream: workspace.EnsureStream},
}
```
`internal/app/app.go:57-60`. Store access mode and identity minting are paired in one value so they cannot disagree (`internal/app/app.go:39-48`).

Behavioral consequences pinned by test: a write-mode open mints; a read-mode open in a never-mutated checkout mints nothing, and a *second* read still finds nothing (`internal/app/app_test.go:144-166`); a read-mode open after a write sees exactly the minted token (`internal/app/app_test.go:171-182`); two worktrees never share an identity (`internal/app/app_test.go:163-165`).

### 2.3 The pair reaching the database

`app.Open` calls `st.AttributeTo(stream.Value())` unconditionally for both modes (`internal/app/app.go:105`). `Store.AttributeTo` pairs the raw token with the store's own workspace id: `s.attribution = model.NewAttribution(streamToken, s.workspaceID)` (`internal/store/store.go:260-262`). An empty token leaves the store unattributed rather than half-attributed (`internal/store/store.go:248-251`). Stamping happens at `recordEvent`, the single insertion point for issue history (`internal/store/store.go:242-247`). `app.Open` is the only caller of `AttributeTo`; `OpenSync`, `RebuildCandidate`, adopt, upgrade, and `OpenLocationForRead` do not stamp — they read, or replay dumps through `insertEventTx`, which preserves the producer's attribution (`internal/store/store.go:252-259`). The interface is `storage.Attributor` (`internal/storage/contract.go:216-218`).

Workspace id source: `Info.WorkspaceID` (`internal/workspace/workspace.go:36`), read from config (`internal/workspace/workspace.go:175`), generated as a UUID at init (`internal/workspace/workspace.go:492`).

Cross-clone proof: attribution survives a real git-remote round trip and a second clone sees the producer's exact pair, never re-stamped (`internal/cli/claims_attribution_test.go:65-129`).

### 2.4 Acting identity — `resolveIdentity` (assignee and event actor)

```go
func resolveIdentity(explicit string) string {
	if sessionID := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")); sessionID != "" {
		return "claude_" + sessionID
	}
	return strings.TrimSpace(explicit)
}
```
`internal/cli/cli.go:1172-1177`.

Precedence, exactly: **`CLAUDE_CODE_SESSION_ID` (trimmed, non-empty) always wins** and yields `"claude_" + sessionID` regardless of any flag; otherwise the caller's explicit value, trimmed; otherwise `""` (`internal/cli/cli.go:1161-1170`).

- Assignee flag on `start`: `--assignee`, help string `"Assignee fallback when CLAUDE_CODE_SESSION_ID is unset (env always wins when set)"` (`internal/cli/cli.go:1274`). Action built as `model.Start{Assignee: resolveIdentity(*assignee)}` (`internal/cli/cli.go:1276`).
- Actor flag: hidden `--by`, empty default, registered by `registerActor` which never exposes the raw pointer — every read passes through `resolveIdentity` (`internal/cli/cli.go:1201-1206`). `os.Getenv("USER")` was deliberately removed as a privacy violation; the fallback is `""`, normalized by the store to the opaque `"unknown"` (`internal/cli/cli.go:1193-1200`).
- Applied in `runTransition`: `actor := resolveActor()` then `Store.Apply(ctx, issueID, storage.Change{Action: action, Actor: actor, Reason: *reason})` (`internal/cli/cli.go:1388-1392`).
- Every relation/label/bulk verb resolves through the same rule; tests pin `label add`, `parent set`, `dep add`, `bulk label add`, `bulk close` to `"claude_" + sessionID` and assert raw `$USER` never lands in `CreatedBy`/`Actor` (`internal/cli/attribution_test.go:33-88`, helpers `:90-141`).
- `displayAssignee("")` renders `"(unassigned)"` (`internal/cli/cli.go:1210-1215`).
- `Start` is the only lifecycle action that rewrites the assignee: `type Start struct{ Assignee string }` (`internal/model/lifecycle/action.go:44-46`), `Target() State` = `InProgress` (`internal/model/lifecycle/action.go:49`). `Issue.Assignee` is orthogonal to the status machine (`internal/model/model.go:88-91`).

Note the interaction pinned by test: because the env var overrides `--assignee`, an e2e test must clear `CLAUDE_CODE_SESSION_ID` or both checkouts flatten to one assignee and a same-state `start` becomes a documented no-op in `store.Apply`, so the claim never transfers (`internal/cli/claims_takeover_e2e_test.go:19-26`).

---

## 3. Evidence assembly

### 3.1 `Evidence`

```go
type Evidence struct {
	members map[model.LaneID][]model.Issue
	events  map[model.LaneID][]model.IssueEvent
}
```
`internal/claims/evidence.go:28-31`.

**`NewEvidence(issues, parents, events)`** (`internal/claims/evidence.go:52-81`):
1. For each issue: `lane := model.LaneOf(issue, parents[issue.ID])`; record `lanes[issue.ID] = lane`; append to `members[lane]` (`:58-62`). An issue absent from `parents`, or mapped to nil, is parentless (`:35-37`).
2. For each event: look up `lanes[event.IssueID]`. If unknown → **error**, verbatim: `claims: event %s belongs to issue %s, which was not among the %d issues supplied: claim derivation needs every issue the events touch, closed ones included` (`internal/claims/evidence.go:66`). Rationale: a `done` on a now-closed ticket can be the sole establishing act (`:39-46`). Pinned by `TestEvidenceRefusesAPartialRead` (`internal/claims/claims_test.go:389-396`).
3. Sort each lane's events by `cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))` — timestamp first, id as the tiebreak; total and stable (`internal/claims/evidence.go:75-79`). Order-independence of the input pinned by `TestDeriveIsOrderIndependent` (`internal/claims/claims_test.go:401-415`).

`Lanes()` returns every lane in the members map, unordered (`internal/claims/evidence.go:84-90`).

### 3.2 `LaneProgress`

```go
type LaneProgress struct { Done, Total int; Active *model.Issue }
```
`internal/claims/evidence.go:99-102`.

`Evidence.LaneProgress(lane)` (`internal/claims/evidence.go:107-120`): iterate `members[lane]`; `Total++` for every member; `Done++` when `issue.State() == model.StateClosed`; `Active` set to a copy of the member whose state is `model.StateInProgress` (last such member wins, since it is assigned unconditionally in the loop). A lane the evidence never saw returns the zero value (`Total 0`), matching `Standings.Of`'s totality convention (`internal/claims/evidence.go:104-106`).

Pinned: closed+in-progress+open lane → `{Done:1, Total:3, Active:T2}` (`internal/claims/evidence_progress_test.go:14-33`); no in-progress member → `Active == nil` (`:39-57`); unseen lane → zero (`:62-71`).

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
`internal/claims/establish.go:37-47`.

- **Exactly two verbs establish**: `start` and `done` (`internal/claims/establish.go:38-39`). `start` is the act of taking work; `done` is the neutral success close, so a checkout that just completed a ticket mid-lane still holds the lane (`internal/claims/establish.go:11-16`).
- `close` (which carries an Outcome: duplicate/superseded/obsolete/wontfix), `reopen`, and the four retention verbs never establish (`internal/claims/establish.go:17-23`).
- `establishes(event)` looks up `establishing[model.ActionName(event.Action)]`; an empty `Action` (plain field update) and an unrecognized verb both read false through the same lookup (`internal/claims/establish.go:54-56`). Pinned by `TestAbsentVerbDoesNotEstablish` (`internal/claims/establish_internal_test.go:42-49`).
- A map rather than a switch so `TestEstablishingCoversEveryAction` can assert every verb in `model.Actions()` is classified and that the map names no retired verb (`internal/claims/establish_internal_test.go:14-23`). `TestOnlyStartAndDoneEstablish` pins the exact classification (`:28-38`).
- Actions vocabulary: `ActionStart = "start"` etc. (`internal/model/lifecycle/lifecycle.go:47`), sealed list at `internal/model/lifecycle/lifecycle.go:137`.

---

## 4. Freshness and the clock

```go
type Freshness struct { Now time.Time; Window time.Duration }
func (f Freshness) Covers(t time.Time) bool { return !t.Before(f.Now.Add(-f.Window)) }
```
`internal/claims/derive.go:20-32`.

- Both the clock reading and the window travel as data; the derivation reads no clock (`internal/claims/derive.go:11-16`).
- **Boundary rule: evidence exactly on the boundary is covered** — `!t.Before(Now-Window)`, i.e. `t >= Now-Window` (`internal/claims/derive.go:25-31`).
- `Covers` is the single place a timestamp is compared against the window (`internal/claims/derive.go:27-29`).

**Configuration**: `claims.freshness_window`, default `"6h"` (`internal/config/config.go:228`). Field `ClaimsConfig.FreshnessWindow time.Duration` with `mapstructure:"-"` — deliberately NOT struct-tag decoded (`internal/config/config.go:56-74`). Parsed once by `parseFreshnessWindow` (`internal/config/config.go:262-266`):
- `time.ParseDuration(raw)` failure → `config: claims.freshness_window must be a duration with a unit, like "72h" or "90m" (got %q): %w` (`internal/config/config.go:92`).
- `window <= 0` → `config: claims.freshness_window must be positive, got %s` (`internal/config/config.go:95`).
- The reason for string parsing: viper weak-decoding a bare `72` would land as 72 **nanoseconds**, positive and passing validation, expiring every claim instantly (`internal/config/config.go:67-72`, `internal/config/config.go:79-88`).

**Where `Now` comes from at runtime**: `claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}` in `gatherClaimContext` (`internal/cli/claims_context.go:101`).

E2E manipulation of the window: a test writes `.lit/config.toml` containing `[claims]\nfreshness_window = "1ms"\n` and sleeps 50 ms (`internal/cli/claims_takeover_e2e_test.go:130-148`).

---

## 5. Local liveness

### 5.1 `claims.LocalCheckouts`

```go
type LocalCheckouts struct { workspace string; live map[string]struct{} }
```
`internal/claims/local.go:27-30`. Constructed by `NewLocalCheckouts(workspaceID, liveStreams)` (`internal/claims/local.go:35-41`).

**Zero value = "this machine has enumerated nothing and therefore proves nothing"**; it voids nothing, which is the "where uncheckable, assume live and let freshness govern" default (`internal/claims/local.go:19-22`). Callers that cannot enumerate must pass the zero value, never a guess (`internal/claims/local.go:32-34`).

**`Void(at model.Attribution) bool`** (`internal/claims/local.go:58-64`):
```go
if !at.Present() || at.Workspace() != l.workspace { return false }
_, alive := l.live[at.Stream()]
return !alive
```
So an event is void iff: attribution present **and** its workspace equals this machine's workspace **and** its stream token is not in the live set. An unattributed event can never be void, which also keeps the zero `LocalCheckouts` inert (its empty workspace would otherwise match an absent pair's empty workspace) (`internal/claims/local.go:55-57`).

Asymmetry, stated at `internal/claims/local.go:11-17`: worktree deletion is a local fact; a claim from a deleted checkout dies here at once, everywhere else waits out the freshness window; a different clone on the same machine carries a different workspace id and is never pruned.

Pinned by `TestLocalCheckoutsScopesTokensToThisWorkspace`: a token of *this* workspace belonging to no live checkout is void; the same token under another workspace id is not; the current checkout's own pair is not (`internal/app/claims_test.go:195-215`).

### 5.2 Enumeration — `workspace.LiveCheckouts`

```go
type Checkout struct { Stream StreamID; Path string; Branch string }
```
`internal/workspace/checkouts.go:27-31`. `Branch` empty for detached HEAD (`internal/workspace/checkouts.go:26`). Nothing is stored; the value is re-derived per enumeration (`internal/workspace/checkouts.go:11-15`).

`LiveCheckouts(cwd)` (`internal/workspace/checkouts.go:61-112`):
1. Runs `git worktree list --porcelain -z` with `context.Background()` (`:65`). On failure the error names the git ≥ 2.36 requirement on **every** failure and is deliberately not routed through `classifyGitError` (`:67-81`).
2. `parseWorktreeList` (`:83`), then `slices.DeleteFunc(records, worktreeRecord.uninhabited)` (`:92`).
3. For each remaining record: `resolvePrivateGitDir(record.path)` then `ReadStream(privateGitDir)`; any failure aborts the **whole** enumeration rather than dropping the checkout, because dropping it would silently assert the checkout is deleted (`:101-108`, rationale `:56-60`).

`worktreeRecord{path, branch, prunable, bare}` (`internal/workspace/checkouts.go:117-122`); `uninhabited() = prunable || bare` (`:132`). A worktree deleted with `rm -rf` leaves its private git dir behind, so filesystem listing would report it alive — git reports it `prunable`; a *locked* worktree is not prunable even with a missing directory (removable media), and this inherits git's judgment (`internal/workspace/checkouts.go:41-50`).

`parseWorktreeList` (`internal/workspace/checkouts.go:191-229`): splits on `\x00`; `strings.Cut(field, " ")` on the FIRST space only; `worktree <path>` opens a record; empty field skipped; an attribute field before any `worktree` field → error `git worktree list --porcelain -z opened with %q, which is not a 'worktree <path>' field` (`:207`). Recognized attributes: `branch` (with `refs/heads/` prefix trimmed), `prunable`, `bare`, `locked`; unknown keys ignored (`:211-218`, documented `:156-167`). `locked` is read but deliberately does not vote on liveness — git already withholds `prunable` from a locked record — and it leaves as `Checkout.Locked` (`:116`), reaching the `claims.Locked` presence via `internal/app/claims.go:65` and `internal/claims/local.go:107-108`. It is the signal `relationOf` reads. Zero records → error `git worktree list --porcelain -z named no worktrees at all`, because git always lists the current worktree and reporting zero would void every claim (`:222-227`).

### 5.3 `app.App.LocalCheckouts` (the boundary)

`internal/app/claims.go:31-37`:
```go
checkouts, err := workspace.LiveCheckouts(a.Workspace.RootDir)
if err != nil { return claims.LocalCheckouts{}, err }
return claims.NewLocalCheckouts(a.Workspace.WorkspaceID, streamTokens(checkouts)), nil
```
On error it returns the zero value **and** the error — it reports what it proved or that it proved nothing (`internal/app/claims.go:26-30`).

`streamTokens(checkouts)` drops any checkout whose `Stream` is not `Present()` (`internal/app/claims.go:53-62`); a never-mutated checkout carries no token, holds no claim, and voids nothing (`internal/app/claims.go:41-45`). Pinned by `TestStreamTokensCountsOnlyMintedIdentities` (`internal/app/claims_test.go:180-188`).

---

## 6. Derivation — the four-legged predicate

`Derive(evidence, fresh, local) Standings` iterates every lane in `evidence.members` and calls `standingOf(members, events[lane], fresh, local)` (`internal/claims/derive.go:37-43`). It writes nothing (`:34-36`).

`standingOf` (`internal/claims/derive.go:58-116`) runs the legs in dependency order **1, 4, 2, 3** (`internal/claims/derive.go:49-55`):

**Leg 1 — the lane is unfinished.** `if !slices.ContainsFunc(members, model.Issue.InPlay) { return Unclaimed{} }` (`internal/claims/derive.go:62-64`). `Issue.InPlay()` = `!lifecycle.Frozen(i.Retention()) && i.State() != model.StateClosed` (`internal/model/model.go:165-167`) — so archived and deleted issues are out of play as well as closed ones. Pinned: all-closed lane → Unclaimed (`internal/claims/claims_test.go:130-140`); sole ticket archived → Unclaimed (`internal/claims/claims_test.go:141-153`).

**Leg 4 — the holder is live as far as this machine can tell.** Applied as a *filter* over events, before leg 2:
```go
admissible := slices.DeleteFunc(slices.Clone(events), func(event model.IssueEvent) bool {
	return local.Void(event.Attribution)
})
```
`internal/claims/derive.go:79-81`. The `slices.Clone` is load-bearing: `DeleteFunc` compacts in place, so without it one derivation would strip events out of the shared `Evidence` and a second derivation over the same reading would silently differ (`internal/claims/derive.go:73-78`). Pinned by `TestDeriveDoesNotConsumeItsEvidence`: one `Evidence`, derived twice — once with a pruning `LocalCheckouts` (→ Unclaimed) and once with the zero value (→ Held) (`internal/claims/claims_test.go:422-438`).

Ordering rationale: leg 4 must run before leg 2 asks which establishing event is *latest*, or a lane would read unclaimed where it should revert to whoever else has standing (`internal/claims/derive.go:50-54`). Pinned by `TestVoidEvidenceFallsThroughToTheNextEstablisher`: A's newer `start` is void, so the lane reverts to B's older one (`internal/claims/claims_test.go:227-235`).

**Leg 2 — the holder produced the latest establishing event.**
```go
establisher, found := latestEstablisher(admissible)
if !found || !establisher.Attribution.Present() { return Unclaimed{} }
holder := establisher.Attribution
```
`internal/claims/derive.go:99-103`. `latestEstablisher` walks the (already totally ordered) slice backwards and returns the last event for which `establishes` is true (`internal/claims/derive.go:119-126`).

**The derivation stops at an unattributed latest establisher; it does NOT scan back to an older attributed ancestor** (`internal/claims/derive.go:87-98`). Rationale: an unattributed `start` says somebody took the lane and the record does not say who — an older attributed event is positively known to be superseded. Pinned by `TestUnattributedLatestStopsRatherThanScanning` (`internal/claims/claims_test.go:212-222`). Contrast with a *void* event, which is disproven rather than unknown and therefore falls through (`internal/claims/local.go:44-53`).

`trails(admissible)` folds the events into two maps (`internal/claims/derive.go:132-142`): `activity[attribution] = event.CreatedAt` (last write wins → each checkout's latest act, because events are oldest-first) and `establishers[attribution] = struct{}{}` for establishing events only.

`tenure := Tenure{By: holder, Since: establisher.CreatedAt, LastActivity: activity[holder]}` (`internal/claims/derive.go:105`).

**Leg 3 — the claim is fresh.**
```go
if !fresh.Covers(tenure.LastActivity) { return Stale{Tenure: tenure} }
return Held{Tenure: tenure, Contested: contestants(holder, activity, establishers, fresh)}
```
`internal/claims/derive.go:112-115`. Freshness is measured from the holder's **last mutation of any kind in the lane**, not from the establishing event, so ordinary commentary carries a claim through a long stretch (`internal/claims/derive.go:108-111`). Pinned by `TestAnyMutationRefreshes`: `start` 80 h ago plus a bare field edit 30 min ago → Held with `Since = -80h`, `LastActivity = -30m` under a 24 h window (`internal/claims/claims_test.go:252-260`).

Stale example: `start` at −72 h plus a field edit at −48 h under a 24 h window → `Stale{By: streamA, Since: -72h, LastActivity: -48h}` (`internal/claims/claims_test.go:178-186`).

**Contest.** `contestants(holder, activity, establishers, fresh)` (`internal/claims/derive.go:155-170`):
- Candidate set = the keys of `establishers` (so **only checkouts with an establishing act** contest; a drive-by comment or grooming edit never does — `internal/claims/derive.go:147-152`).
- Skip candidate if `candidate == holder`, or `!candidate.Present()`, or `!fresh.Covers(activity[candidate])` (`:158-160`) — a rival whose own evidence aged out is no longer contesting.
- Sort: most-recently-active first (`activity[b].Compare(activity[a])`), tie-broken by `strings.Compare(a.Stream(), b.Stream())` (`:163-168`).
- Returns `[]model.Attribution{}` (non-nil empty) when nobody contests (`:156`).
- Contest is an annotation, not a state: routing is unaffected and the holder remains the holder (`internal/claims/standing.go:41-46`).

Pinned: A starts at −3 h, B starts at −1 h → `Held{By: B, Contested: [A]}` (`internal/claims/claims_test.go:293-304`); A's start at −200 h with B at −1 h → Held by B, no contest (`internal/claims/claims_test.go:308-316`); B's edit + archive + close after A's start → still Held by A with no contest (`internal/claims/claims_test.go:276-288`).

**Cold start.** A repository whose whole history predates attribution derives `Unclaimed` for every lane (`internal/claims/claims_test.go:373-384`), which is exactly the pre-claims behavior (`internal/claims/standing.go:19-23`, `internal/claims/derive.go:96-98`).

**Foreign workspaces never pruned**: an event from `ws-elsewhere` remains Held even when this machine enumerates zero live streams for `ws-local` (`internal/claims/claims_test.go:240-247`).

**Grid summary** (all under a 24 h window, `internal/claims/claims_test.go:108-205`):

| dropped leg | fixture | result |
|---|---|---|
| none | `start` by A at −2 h, both streams live | `Held{A, Since:-2h, LastActivity:-2h}` |
| 1 (closed) | both tickets closed | `Unclaimed` |
| 1 (archived) | sole open ticket archived | `Unclaimed` |
| 2 (no establishing verb) | `reopen`, `archive`, `close`, bare edit | `Unclaimed` |
| 2 (unattributed latest) | A's start at −3 h, unattributed start at −1 h | `Unclaimed` |
| 3 (stale) | start −72 h, edit −48 h | `Stale{A}` |
| 4 (checkout gone) | start by A, live set = {B} | `Unclaimed` |

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
`internal/app/app.go:13-24`. `Stream` documented as: always present under `AccessWrite` (minted on the checkout's first mutating command); present under `AccessRead` only if an earlier mutating command minted it, and its absence is the honest report that this checkout has produced no work evidence and therefore holds no claim (`internal/app/app.go:16-23`).

### 7.2 `AccessMode` / `accessContract` / `accessContracts`

Covered in §2.2. Type declarations at `internal/app/app.go:30-35`, `:45-48`, `:57-60`.

### 7.3 `Open(ctx, cwd, mode) (*App, error)`

`internal/app/app.go:68-107`. Ordered orchestration:
1. `contract, known := accessContracts[mode]`; unknown (including the zero value `""`) → `fmt.Errorf("invalid access mode %q", string(mode))` (`:69-74`). The map lookup is both the validity check and the dispatch (`:65-67`).
2. `workspace.Resolve(cwd)` → error returned as-is (`:75-78`).
3. `engine.Open(ctx, contract.mode, ws.DatabasePath, ws.WorkspaceID)` → error returned as-is (`:79-82`).
4. `contract.resolveStream(ws.PrivateGitDir)` — resolved **after** the store opens, so a command that cannot reach its store mints nothing (`:83-86`).
5. On identity failure: `return nil, errors.Join(err, st.Close())` — the store must be closed because `Store.Close` also releases the workspace lock; joined so a stranded lock is visible too (`:87-96`).
6. `st.AttributeTo(stream.Value())` — called unconditionally for both modes; only the value varies (`:97-105`).
7. `return &App{Workspace: ws, Store: st, Stream: stream}, nil` (`:106`).

Validation/behavior pinned by test:
- `AccessWrite` bootstraps a missing database; `AccessRead` fails with an error containing `"not initialized"` (`internal/app/app_test.go:25-58`).
- Read mode accepts the database write mode bootstrapped (`internal/app/app_test.go:62-78`).
- `""` and `"admin"` both fail with `"invalid access mode"` (`internal/app/app_test.go:83-95`).
- A damaged token file makes `Open` fail with a `"malformed"` diagnosis and the store must be released — proven by a repaired second open succeeding (`internal/app/app_test.go:193-230`).

No events are emitted by `app`; the package publishes no event/observer surface.

### 7.4 `OpenLocationForRead(ctx, loc) (storage.Store, error)`

`internal/app/app.go:125-131`. Opens a store at an already-derived `workspace.Location`, bypassing cwd git resolution entirely — the cross-project open primitive used by aggregation over many stores (`:109-114`). Reads the foreign store's `workspace_id` from its own `config.json` via `workspace.ReadConfig(loc.ConfigPath)` — a pure read that never writes the foreign store (`:122-124`, `:126-129`), then `engine.Open(ctx, engine.ReadOnly, loc.DatabasePath, cfg.WorkspaceID)` (`:130`). Always `ReadOnly`, so a foreign store gets the shared lock and never a second read-write engine the embedded Dolt driver would reject as "database is read only" (`:115-121`). **It mints no identity and calls no `AttributeTo`.**

### 7.5 `(*App).Close() error`

`internal/app/app.go:133`: `return a.Store.Close()`.

### 7.6 `(*App).LocalCheckouts() (claims.LocalCheckouts, error)`

`internal/app/claims.go:31-37`. Covered in §5.3. Placement rationale (effects at the boundary, `internal/claims` importing only `internal/model`) at `internal/app/claims.go:10-19`; workspace-id scoping as both a correctness and a privacy property at `:21-24`.

### 7.7 `streamTokens(checkouts []workspace.Checkout) []string` (unexported)

`internal/app/claims.go:53-62`. Covered in §5.3.

That is the entire `internal/app` surface: `App` (3 fields), `AccessMode` + 2 constants, `accessContract`, `accessContracts`, `Open`, `OpenLocationForRead`, `Close`, `LocalCheckouts`, `streamTokens`.

---

## 8. Gathering the claim context in the CLI

`type claimContext struct { standings claims.Standings; evidence claims.Evidence; self model.Attribution; addresses map[model.Attribution]workspace.Checkout }` (`internal/cli/claims_context.go:33-38`). `addresses` never reaches the shared database and lives only for the process's lifetime (`internal/cli/claims_context.go:22-32`).

`gatherClaimContext(ctx, stdout, ap)` (`internal/cli/claims_context.go:43-108`), in order:
1. `config.Load(pathspec.New(ap.Workspace.RootDir))` (`:44-47`).
2. `ap.Store.ListIssues(ctx, storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})` — **both flags set**, because a lane's establishing event can sit on a deleted or archived issue; with the zero-value filter, a repository with even one deleted issue that ever carried an event made `NewEvidence` fail outright on every `next` and `backlog` (`:48-60`).
3. `ap.Store.GetRelationsByIDs(ctx, ids)` → `parents[issue.ID] = relations[issue.ID].Parent` (`:61-72`).
4. `ap.Store.ListAllEvents(ctx)` (`:73-76`).
5. `claims.NewEvidence(allIssues, parents, events)` (`:77-80`).
6. `workspace.LiveCheckouts(ap.Workspace.RootDir)`:
   - **On error**: prints to stdout, verbatim, `warning: could not enumerate local checkouts (%v) — claim liveness check and local addresses skipped, freshness alone governs\n`, and leaves `local` as the zero `claims.LocalCheckouts` and `addresses` nil. A failure to print the warning aborts the whole gather (`:89-95`).
   - **On success**: `local = claims.NewLocalCheckouts(ap.Workspace.WorkspaceID, checkoutStreamTokens(checkouts))` and `addresses = addressesByAttribution(ap.Workspace.WorkspaceID, checkouts)` (`:96-99`).
7. `fresh := claims.Freshness{Now: time.Now(), Window: cfg.Claims.FreshnessWindow}`; `standings := claims.Derive(evidence, fresh, local)` (`:101-102`).
8. `self := model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)` — a never-minted stream collapses to the zero Attribution, which is exactly "no live claims", with no branch needed (`:103-106`).

`checkoutStreamTokens` mirrors `app.streamTokens`: skips checkouts without a present stream (`internal/cli/claims_context.go:114-122`). `addressesByAttribution` indexes live checkouts by `model.NewAttribution(checkout.Stream.Value(), workspaceID)`, skipping tokenless checkouts (`internal/cli/claims_context.go:128-136`).

Callers: `next` (`internal/cli/next.go:65`), `workable`/`backlog` runner (`internal/cli/workable.go:160`), `authorizeStart` (`internal/cli/claims_takeover.go:132`), `reportContestedLanes` (`internal/cli/claims_contest_report.go:33`).

---

## 9. Gates: which operations consult claims, and what happens on conflict

### 9.1 `lit start` — the takeover gate (the only write gate)

`transitionSpec.authorize` is an optional hook that runs after the action is built and **before** `Store.Apply`, and may abort the transition by returning an error; only `start` supplies one, the other seven transitions use `noAuthorize` (`internal/cli/cli.go:1346-1353`, `noAuthorize` at `:1356-1360`). Wired at `internal/cli/cli.go:1383-1388`, bound at `:1462` and invoked at `:1491`. The flag: `--take`, help string `"Confirm taking over a lane another checkout claims right now (required for non-interactive callers; an interactive terminal is prompted instead)"` (`internal/cli/cli.go:1384`).

**`classifyTakeover(standing, self) takeoverRequirement`** — pure, no I/O (`internal/cli/claims_takeover.go:110-119`). It does not read the standing itself: it switches on `relationOf(standing, self)`, the same relation routing admits on, so the gate and the router cannot disagree about whose lane it is.

| standing | condition | relation | requirement |
|---|---|---|---|
| `Held` | `s.By == self` | `laneOurs` | `takeoverNone` |
| `Held` | otherwise | `laneHeldForeign` | `takeoverFreshConfirm` |
| `Stale` | `s.By == self` | `laneOurs` | `takeoverNone` |
| `Stale` | `s.Holder == claims.Locked` | `laneHeldForeign` | `takeoverFreshConfirm` |
| `Stale` | otherwise | `laneStaleForeign` | `takeoverStaleInformed` |
| `Unclaimed` (default arm) | — | `laneUnclaimed` | `takeoverNone` |

The `claims.Locked` row is the one a reader is likeliest to miss: an expired claim whose holder's worktree is locked is gated as a fresh hold, not waved through with a warning (`:94`).

Sealed int enum: `takeoverNone`, `takeoverStaleInformed`, `takeoverFreshConfirm` (`internal/cli/claims_takeover.go:27-33`). Pinned across all five standings by `TestClassifyTakeover` (`internal/cli/claims_takeover_test.go:14-33`).

**`authorizeStart(ctx, stdout, ap, issueID, prior, take)`** (`internal/cli/claims_takeover.go:132-154`):
1. `ap.Store.GetRelationsByIDs(ctx, []string{issueID})` → `lane := model.LaneOf(prior, relations[issueID].Parent)` (`:133-137`).
2. `gatherClaimContext` (`:138-141`).
3. Dispatch on `classifyTakeover(cc.standings.Of(lane), cc.self)` (`:142`):
   - `takeoverNone` → `return nil`; the happy path pays one extra evidence gather and nothing else (`:143-144`, rationale `:124-127`).
   - `takeoverStaleInformed` → `printStaleProvenance` (`:145-146`).
   - `takeoverFreshConfirm` → `confirmFreshTakeover` (`:147-148`).
   - default (unreachable) → `fmt.Errorf("claims: %s has no recognized takeover requirement", issueID)` (`:149-152`).

**`claimLineOrPanic`** reuses `formatClaimLine(cc, lane, time.Now())`; `ok == false` → error `claims: %s has a takeover requirement on %v but no claim line to show` (`internal/cli/claims_takeover.go:164-170`).

**`printStaleProvenance`** — proceeds unprompted and prints `"%s — check for unmerged branches or PRs on this lane before building on it\n"` (`internal/cli/claims_takeover.go:111-118`). Checking for unmerged branches or PRs is left to the taking agent; lit stays ignorant of git and the forge (`:109-110`).

**`confirmFreshTakeover(stdout, cc, lane, take)`** (`internal/cli/claims_takeover.go:195-218`):
- **Non-interactive** (`!isTerminal(stdout)`, the same signal `openOrPrintWorkflowFile` uses — `internal/cli/workflows_edit.go:160`):
  - `take == false` → **refusal**: `fmt.Errorf("%s — this lane is claimed and active; pass --take to confirm the takeover", line)` (`:135-137`).
  - `take == true` → prints `"%s — taking over (--take)\n"` and proceeds (`:138-139`).
- **Interactive**: prints `"%s\ntake over this lane? [y/N] "`, reads a line from `os.Stdin` via `bufio.NewReader(os.Stdin).ReadString('\n')` (`:141-147`). A read error other than `io.EOF` → `fmt.Errorf("read takeover confirmation: %w", err)`. The answer is accepted iff `strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y")`; otherwise → `fmt.Errorf("takeover declined")` (`:148-150`).

E2E, over two real clones and a real git remote (`internal/cli/claims_takeover_e2e_test.go:18-80`): alpha starts and pushes; bravo's `start` without `--take` fails with an error containing both `--take` and `claimed`; the same command with `--take` prints `"taking over"`; and starting the now-bravo-held lane again produces neither `"claimed"` nor `"--take"` in the output. Stale path (`:88-126`): with `freshness_window = "1ms"` and a 50 ms sleep, bravo's plain `start` succeeds and prints both `"check for unmerged branches or PRs"` and `"stale"`.

### 9.2 `lit next` — claim-aware routing (a read gate)

Registered `app.AccessRead`; it performs no writes (`internal/cli/register.go:484-485`). Flags (`internal/cli/next.go:31-40`): `--assignee`, `--type`, `--status` (`open|in_progress`), `--labels`, and `--all`, help string `"Ignore the focus scope and route over the whole queue"`. No `--limit`, no `--columns`. `--continue` is retired: every spelling of it is wrapped at the parse boundary as `UnsupportedError{Feature: "--continue"}` carrying ``--continue is retired; claim routing already keeps `lit next` in your checkout's own epic first — run `lit next` with no flag`` (`internal/cli/flagset.go:134-138`).

The leaf gathers rows, relation details and the focus scope, then the claim context, then routes: `routeNext(rows, details, cc.standings, cc.self, focus.scopeFor(*all))` (`internal/cli/next.go:58-71`). `focusScope.scopeFor(all)` returns the zero `focusScope` — the whole queue — when `--all` is set, so the flag picks a value and every stage after it stays unconditional (`internal/cli/ready_state.go:507-516`).

**`NextOutcome`** is a sealed sum (`internal/cli/next_route.go:26`, markers `:179-184`) — **six** cases:
- `ServedFromClaim{Row}` — a ready ticket in a lane this checkout already holds. Routing step 1; no new claim is established, so nothing is announced (`:28-30`).
- `ResumedOwnWork{Row}` — a ticket already in flight in a lane this checkout holds, handed back to its holder. Routing step 1 for work that is started rather than startable; nothing is claimed and nothing is begun, so it reports a state rather than an act (`:32-40`).
- `ServedFromEpicLane{Row, Lane model.LaneID}` — a pick from a different lane of the same epic this checkout already holds a lane in. Routing step 2. Two fields exactly: the epic is `Lane.Epic()`, and carrying it beside the lane would be two clocks for one fact (`:42-53`).
- `ServedFromNewLane{Row, Lane model.LaneID}` — a ready ticket in a lane this checkout does **not** hold. **Two** steps produce it: the global pool (step 4) and the on-path dependency gating one of our own blocked rows (step 1b). `Lane` is the `model.LaneID`, not its `String()`: stringifying here would throw away the discriminator the renderer needs to tell a lane worth naming from one that would only repeat the ticket (`:55-75`).
- `Exhausted{Epics []string, Blocked []rowReach}` — the checkout's own claimed epic(s) have open work with none of it reachable. Routing step 3 (`:77-92`).
- `NoWork{Unreachable []rowReach}` — the global pool handed back nothing (`:162-177`).

`Exhausted` and `NoWork` are themselves `error` implementations and travel outward **as themselves** rather than being rendered into a generic error, which is what keeps the exit-code and reason sinks reading the routing verdict instead of a copy that could drift (`:83-88`, `internal/cli/next.go:109-121`).

**`capacityFor(row, standing, self) capacity`** — the admission rule; pure, total, no I/O (`internal/cli/next_route.go:229-250`). Four capacities: `routeAround` (not this checkout's to take right now), `serveWork`, `resumeWork`, `takeoverWork` (`:196-208`). It reads `readiness := ClassifyReadiness(row.Annotations)`, `relation := relationOf(standing, self)`, and `started := row.State() == model.StateInProgress`, with `takeable := (started && readiness.IsOrphaned()) || (!started && readiness.IsReady())`. Evaluated top-down:

| # | condition | capacity |
|---|---|---|
| 1 | `relation == laneOurs` and `started` | `resumeWork` |
| 2 | `relation == laneOurs` and `readiness.IsReady()` | `serveWork` |
| 3 | `relation == laneOurs`, otherwise | `routeAround` |
| 4 | `!takeable`, or `relation == laneHeldForeign` | `routeAround` |
| 5 | `started`, or `relation == laneStaleForeign` | `takeoverWork` |
| 6 | otherwise | `serveWork` |

`laneStaleForeign` yields `takeoverWork`, and routing steps 2 and 4 both accept the set `{serveWork, takeoverWork}` — so a stale foreign lane is a reachable bare `lit next` target. `laneHeldForeign` — a lane another checkout holds fresh — is the one relation routed around on ownership alone. The `laneRelation` constants are at `internal/cli/claims_takeover.go:46-60`; `relationOf` at `:68-101`, where a `claims.Stale` standing whose `Holder == claims.Locked` reports `laneHeldForeign` rather than `laneStaleForeign` (`:80-97`).

**`ownScope(standings, self) (map[model.LaneID]bool, map[string]bool)`** (`internal/cli/next_route.go:265-278`): every lane whose `relationOf(standing, self)` is `laneOurs`, plus each such lane's non-empty `Epic()`. It reads the **standings**, not the gathered rows — the rows are already narrowed by `--type/--labels/--assignee`, and deriving ownership from them let a display filter empty the set and drop the whole self-aware branch (`:255-264`).

**`routeNext(rows, details, standings, self, scope focusScope) NextOutcome`** — five parameters (`internal/cli/next_route.go:307-390`). Closures: `laneOf`, `verdict` (= `capacityFor`), `reachFor` (= `reachOf`), and `pickFrom(from, inScope, accept ...capacity)`, which keeps the first row of `from`, in the gather's composite-rank order, that sits in an admitted lane and carries one of the accepted verdicts (`:308-340`). `accept` is a **set**, never a preference order: composite rank is the only tiebreak routing applies, and ranking capacities against each other would pass over the backlog's #1 row, an orphan, for a lower-ranked leaf that needed no takeover (`:320-325`).

Precedence, with `ownLanes, ownEpics := ownScope(standings, self)` and `mine := func(lane) bool { return ownLanes[lane] }` (`:342-343`). If `len(ownLanes) > 0`:
1. **Step 1** — our own lanes, accepting `{serveWork, resumeWork}`, whichever the backlog ranks first. `resumeWork` → `ResumedOwnWork{Row}`; `serveWork` → `ServedFromClaim{Row}` (`:345-352`).
2. **Step 1b** — `onPathDependency(rows, laneOf, mine, reachFor)`, a dependency outside our lanes that gates one of them → `ServedFromNewLane{Row: dep, Lane: laneOf(dep)}`. It establishes a claim on a lane we do not hold, so it is announced as one (`:353-359`).
3. **Step 2** — the rest of our epic, in lanes we do not already hold: predicate `lane.Epic() != "" && ownEpics[lane.Epic()] && !mine(lane)`, accepting `{serveWork, takeoverWork}` → `ServedFromEpicLane{Row, Lane}` (`:360-366`).
4. **Step 3** — `Exhausted{Epics: slices.Sorted(maps.Keys(ownEpics)), Blocked: gatingDependencies(rows, laneOf, mine-or-ourEpic, reachFor)}` (`:367-373`). Loud, and never a hop: **exhaustion does not fall through to the global pool.**

**Step 4** is reached only by a checkout holding no lanes, which starts there directly: `pool, offPath := scope.partition(rows)`, then `pickFrom(pool, <every lane>, serveWork, takeoverWork)` → `ServedFromNewLane{Row, Lane}`, else `NoWork{Unreachable: append(passedOver(pool, reachFor), withheldByScope(offPath)...)}` (`:376-389`). The scope-withheld rows travel into the diagnostic rather than vanishing, because "nothing is startable" and "nothing on your focus path is startable" are different answers (`:380-384`). `withheldByScope` stamps them `reachOffFocusPath` at the one place that applied the scope (`:392-403`); `passedOver` classifies every row the pool walk went through, all `routeAround` by construction (`:405-424`).

Steps 1-3 walk every gathered row; step 4 walks the focus-scoped pool. The row set is passed to `pickFrom` explicitly at each call so that difference stays visible (`:326-329`). The focus scope narrows step 4 and nothing else (`:294-301`).

**`reachKind`** (`internal/cli/next_route.go:106-131`) is what one row is to this checkout right now — four classifications plus the pool walk's own, and a bound: `reachTakeable`, `reachHeldFresh`, `reachNotReady`, `reachOutOfView`, `reachOffFocusPath`, `reachKindCount`. A bool here read "takeable or not", so a row outside the run's filtered view, or one not startable itself, rendered as the one reason the message named: claimed by another checkout (`:94-105`). `rowReach{ID string; Row annotation.AnnotatedIssue; Kind reachKind}` (`:133-139`).

**`reachOf(row, gathered, standing, self)`** (`:150-160`): `!gathered` → `reachOutOfView`; `capacityFor(...) != routeAround` → `reachTakeable`; `relationOf(...) == laneHeldForeign` → `reachHeldFresh`; otherwise `reachNotReady`. Exhaustion asks it of the dependencies gating our scope; an empty global pool asks it of every row the walk went past (`:101-102`).

**`gatingDependencies`** (`:433-454`): the distinct open dependency ids, in rank order, gating the open rows whose lane `inScope` admits, each already carrying its `reachKind`. **`onPathDependency`** (`:469-476`) returns the first of those whose `Kind == reachTakeable`. A same-lane gate never reaches here: it shares the blocked row's lane, so step 1 already served or resumed it (`:459-460`).

**`describeReach(rows, lead, notes)`** (`:561-575`) renders `"<lead><ids> (<note>)"` for each kind that has rows, joined by `"; "`, in `reachKind`'s declaration order. `nameIDs` names at most `maxNamedPerKind = 12` ids and states how many it left out (`:540-553`). `reachNotes` is `[reachKindCount]string`, indexed by the kind itself (`:507`).

`exhaustedNotes` (`:521-526`), exact strings:
- `reachTakeable`: ``on your path and yours to take — `lit start` it``
- `reachHeldFresh`: `on your path but claimed by another checkout right now`
- `reachNotReady`: ``on your path but not startable right now — `lit show` it``
- `reachOutOfView`: ``on your path but outside this view — `lit show` it``

`poolNotes` (`:527-531`), exact strings:
- `reachHeldFresh`: `in progress or claimed in a lane another checkout holds right now`
- `reachNotReady`: `not startable — blocked by a dependency, or in flight and not abandoned`
- `reachOffFocusPath`: ``off the focus path this run answered over — `lit next --all` to route over the whole queue``

**`Exhausted.Error()`** (`:480-489`). `scope` is `fmt.Sprintf("epic(s) %s", strings.Join(o.Epics, ", "))` when epics are named, else `"your claimed lane(s)"`. The command is in **backticks** in both arms:
- `len(o.Blocked) == 0`: ``no ready work in %s — nothing else is queued behind what's already in progress; picking up other work is a deliberate re-focus, not a bare `next` ``
- otherwise: ``no ready work in %s — %s; picking up other work is a deliberate re-focus, not a bare `next` ``, the middle being `describeReach(o.Blocked, "blocked on ", exhaustedNotes)`.

**`NoWork.Error()`** (`:587-603`):
- `len(o.Unreachable) == 0` → `no ready work`.
- `o.withheld()` — any row of kind `reachOffFocusPath` (`:608-615`) → `no ready work on the focus path — the backlog is not empty, and each row below says why this run did not serve it: %s`.
- otherwise → `no ready work — the backlog is not empty, but nothing in it is startable here: %s`.

The `%s` in both non-empty arms is `describeReach(o.Unreachable, "", poolNotes)`.

**Exit code and reason.** `ExitNoWork = 6` (`internal/cli/exit.go:30`). **Both** `Exhausted` and `NoWork` map to it (`internal/cli/exit.go:116-128`): distinct from `ExitGeneric` because a caller looping `lit next` has to tell "stop, there is nothing for you" from "lit is broken", and under one code its only way to do that was to parse the English; not `ExitOK`, because for `lit next` 0 means "a ticket is on stdout", and exiting 0 with no row would hand the caller a success-shaped void (`internal/cli/exit.go:18-29`). Reasons: `scope_exhausted` for `Exhausted`, `no_ready_work` for `NoWork` (`internal/cli/error_output.go:105-118`).

**`startAdvice(row, lane, holder)`** (`internal/cli/next.go:183-196`) — the line every pick that would establish a claim prints above its row: what running `lit start` would lock, never what this command did. `lit next` claims nothing and starts nothing; the function was `claimAnnouncement` and the rename is the fix, an announcement reporting being the one thing a read-only command must not do (`internal/cli/next.go:89-93`, `:159-162`). `described, named := lane.Describe()`; exactly four sentences:
- in progress, lane not named: ``%s is in progress and %s — run `lit start %s` to take it over`` (Row.ID, state, Row.ID).
- in progress, lane named: ``%s is in progress and %s — run `lit start %s` to take over %s`` (Row.ID, state, Row.ID, described).
- not in progress, lane not named: ``run `lit start %s` to claim it`` (Row.ID).
- not in progress, lane named: ``run `lit start %s` to claim %s`` (Row.ID, described).

`state` is `inFlightState(holder)` (`internal/cli/next.go:149-157`): `claims.Locked` → `claimed by a locked worktree whose claim has gone stale`; `claims.Present` → `stale, though its holder's worktree is still on disk`; otherwise `abandoned`. `holder` is `expiredHolder(cc.standings.Of(lane))` — the `Stale` standing's `Holder`, and `claims.Unprovable` for every other standing (`internal/cli/claims_render.go:89-94`). The two verbs spell their sentences out separately rather than sharing one with the object substituted, because English puts the pronoun in different places: "claim it", but "take it over" (`internal/cli/next.go:177-182`).

**`LaneID.Describe() (string, bool)`** (`internal/model/model.go:255-263`) — three cases:
- solo lane → `("", false)`. A solo lane is the ticket that names it, so any phrase for it only repeats what the surrounding sentence already said (`:248-251`).
- empty key → `(fmt.Sprintf("the default lane of epic %s", l.epic), true)`.
- otherwise → `(fmt.Sprintf("lane %s of epic %s", l.key, l.epic), true)`.

**`renderNextOutcome(w, outcome, details, cc)`** (`internal/cli/next.go:94-135`):
- `ServedFromClaim` → no announcement at all.
- `ResumedOwnWork` → `"%s is already in progress in a lane you hold — continue where you left off\n"` (Row.ID).
- `ServedFromEpicLane` → `startAdvice(o.Row, o.Lane, expiredHolder(cc.standings.Of(o.Lane)))` + `" (a second lane of an epic you already hold a lane in)\n"`.
- `ServedFromNewLane` → `startAdvice(o.Row, o.Lane, expiredHolder(cc.standings.Of(o.Lane)))` + `"\n"`.
- `Exhausted`, `NoWork` → returned as themselves; nothing printed.
- default → `panic(fmt.Sprintf("renderNextOutcome: unhandled NextOutcome %T", outcome))`.

For the four served cases the announcement is written only when non-empty, then `lane := model.LaneOf(row.Issue, details[row.ID].Parent)` and `printNextSummary(w, row, cc, lane)` (`internal/cli/ready_state.go:663`); finally `nextPulledOccasion(row.Issue)` is returned and dispatched to workflows (`internal/cli/next.go:125-134`, `:75`).

### 9.3 `lit sync reconcile` — the contest report (a read gate on merge)

`reportContestedLanes(ctx, stdout, ws, syncStore)` (`internal/cli/claims_contest_report.go:32-58`):
- Builds a temporary `&app.App{Workspace: ws, Store: syncStore}` — note: **no `Stream`**, so `cc.self` is the zero Attribution for this call (`:33`, cf. `internal/cli/claims_context.go:106`).
- `lanes := contestedLanes(cc.standings)`; if empty, returns nil silently (`:37-40`).
- Header, verbatim: `contested: evidence from more than one checkout just met for these lanes —` (`:41`).
- Per lane: `"  %s: %s\n"` with `lane` (via `LaneID.String()`) and `formatClaimLine(cc, lane, now)` where `now = time.Now()` taken once for the whole report (`:44-56`).
- `ok == false` from `formatClaimLine` → error `contested lane %s reported no claim line — standings and rendering disagree` (`:46-52`).

`contestedLanes(standings)` (`internal/cli/claims_contest_report.go:63-74`): every lane whose standing is `claims.Held` with `len(held.Contested) > 0`; returns a non-nil empty slice otherwise; sorted by `strings.Compare(a.String(), b.String())`. Pinned by `TestContestedLanesFiltersAndSorts` (Unclaimed, Stale, and uncontested Held all drop out; survivors sorted) (`internal/cli/claims_contest_report_test.go:14-37`) and `TestContestedLanesEmptyForNoContest` (`:43-51`).

**Call sites**: only two — after `storage.SyncReconcileLinearized` (`internal/cli/sync_reconcile_cmd.go:519`) and after `storage.SyncReconcileCombined` (`internal/cli/sync_reconcile_cmd.go:543`), the two states where histories actually merged (`internal/cli/sync_reconcile_cmd.go:441`). Not called for prose-pending, unrelated-histories, or not-diverged outcomes.

E2E: two clones partition-start the same lane; `lit sync reconcile` on bravo prints output containing `"contested"` and the ticket id (`internal/cli/claims_contest_report_e2e_test.go:16-61`). Negative half: an ordinary reconcile with no shared lane never mentions `"contested"` (`:67-104`).

### 9.4 Surfaces that render but do not gate

- `lit backlog` — `printBacklogContext` prints the claim line, indented, after the `in_progress:` line and before `unblocks:` (`internal/cli/backlog.go:92-96`). `backlogView` is the only `workableView` preset (`internal/cli/workable.go:87-95`), and its render function is `printBacklogOutput(w, columns, issues, details, cc)` (`internal/cli/backlog.go:32`).
- `printInlineDeps` — the shared epic/depends-on/claim/unblocks block used by `lit next`'s summary, printing the claim line between `depends on` and `unblocks` (`internal/cli/ready_state.go:601-614`). `printNextSummary` calls it after the issue's column line (`internal/cli/ready_state.go:550-557`).

No other command consults `claims.Standings`: the only readers of `cc.standings` / `cc.self` outside `internal/cli/claims_*.go` are `next.go:69` (routing) — everything else consumes `cc` only for rendering (`internal/cli/workable.go:54`, `internal/cli/backlog.go:32,72`, `internal/cli/ready_state.go:550,601`).

---

## 10. Rendering

### 10.1 `formatClaimLine(cc, lane, now) (string, bool)`

`internal/cli/claims_render.go:23-44`. Returns `("", false)` for anything that is not `Held` or `Stale` — an Unclaimed lane renders **no line at all**, not an empty or placeholder one (`:36-37`, rationale `:14-15`; pinned `internal/cli/claims_render_test.go:61-67`).

- `Held` → `line = claimPrefix(tenure.By, false, cc)`; if `len(standing.Contested) > 0`, append `fmt.Sprintf(" · contested by %s", strings.Join(shortStreams(standing.Contested), ", "))` (`:27-33`).
- `Stale` → `line = claimPrefix(tenure.By, true, cc)` (`:34-35`).
- Then `parts := []string{line, humanizeCoarseDuration(now.Sub(tenure.LastActivity)) + " ago"}`; if `formatLaneProgress(cc.evidence.LaneProgress(lane))` is non-empty, append it; join with `" · "` (`:39-43`).

**Two tiers**: the dossier (holder badge, freshness, lane progress) comes entirely from `cc.evidence` and `cc.standings` — the shared, synced data — so it renders identically on any clone; the address renders only when `cc.addresses` resolves the holder to a live worktree **this machine** enumerated (`internal/cli/claims_render.go:16-22`).

### 10.2 `claimPrefix(by, stale, cc)`

`internal/cli/claims_render.go:52-69`:
- `tag = " (stale)"` when `stale`, else `""`.
- If `cc.addresses[by]` resolves: `branch := checkout.Branch`; if empty, `branch = "detached HEAD"`; returns `fmt.Sprintf("claimed here%s: %s (%s)", tag, checkout.Path, branch)`.
- Otherwise: `state := "elsewhere"`, or `"stale"` when `stale`; returns `fmt.Sprintf("claimed: stream %s (%s)", shortStream(by), state)`.
- A **stale** claim from a still-live local worktree still resolves to that worktree's address; `stale` controls only the label (`:48-51`; pinned `internal/cli/claims_render_test.go:142-161`, expecting `claimed here (stale): ../links-wt-pgct (detached HEAD)`).

### 10.3 `formatLaneProgress(progress)`

`internal/cli/claims_render.go:76-84`:
- `Total == 0` → `""`.
- `Active != nil` → `fmt.Sprintf("%s in progress, %d/%d done", progress.Active.ID, progress.Done, progress.Total)`.
- else → `fmt.Sprintf("%d/%d done", progress.Done, progress.Total)`.

### 10.4 `shortStream` / `shortStreams`

`internal/cli/claims_render.go:92-107`: truncates the stream token to the first 8 characters when longer (`labelLen = 8`). Explicitly a display nicety, not a privacy measure — the full token is already opaque (`:86-91`).

### 10.5 `humanizeCoarseDuration`

`internal/cli/output.go:451-462`, buckets:
- `>= 48h` → `"%d days"` (`int(d/(24*time.Hour))`)
- `>= 2h` → `"%d hours"` (`int(d/time.Hour)`)
- `>= 2m` → `"%d minutes"` (`int(d/time.Minute)`)
- else → `"under a minute"`

### 10.6 Rendering behavior pinned by test

- Dossier without any local address: line says `"elsewhere"`, carries `"1/2 done"`, names `"active-ticket in progress"`, and reads `"2 hours ago"` (`internal/cli/claims_render_test.go:74-97`).
- With an address entry: `"claimed here: ../links-wt-pgct (links-claims-1ihf.11)"`; the same standing rendered without addresses must not carry the path and must say `"elsewhere"` (`internal/cli/claims_render_test.go:103-137`).
- Contested Held: line contains `"contested by " + shortStream(contestant)` (`internal/cli/claims_render_test.go:166-181`).

---

## 11. Privacy invariants stated in code

- Both halves of `Attribution` are opaque by mandate; nothing user-, host-, or path-shaped may travel there, because the database syncs to shared remotes; resolving a token to a physical checkout happens only on the machine that owns it (`internal/model/model.go:624-627`).
- `StreamID` is deliberately meaningless — no directory name, hostname, or username material (`internal/workspace/stream.go:40-49`).
- `--by`'s old `os.Getenv("USER")` default was removed as a documented-invariant violation; the fallback is `""` → the opaque `"unknown"` (`internal/cli/cli.go:1196-1200`).
- `Checkout.Path` / `Checkout.Branch` stay on the local machine (`internal/workspace/checkouts.go:21-26`); `claimContext.addresses` never reaches the shared database and lives only for the process (`internal/cli/claims_context.go:30-32`).
- A different clone of the same repository on the same machine carries a different workspace id, so this machine's enumeration never speaks to its claims (`internal/app/claims.go:21-24`).

---

## 12. End-to-end acceptance already proven in tests

`TestDeletedCheckoutReleasesItsClaimHereAndAgesOutElsewhere` (`internal/app/claims_test.go:101-173`), driven against real git worktrees and the real store:
1. A linked worktree opens `AccessWrite` (minting its token), creates an issue, and applies `model.Start{Assignee: "worker"}`; `lane = model.LaneOf(issue, nil)` (`:108-125`).
2. The primary reads: `Derive(evidence, {Now: time.Now(), Window: 24h}, local).Of(lane)` is `Held` with `held.By.Stream() == workerToken` (`:129-144`).
3. `git worktree remove --force` (`:146`).
4. The primary's **very next** derivation over a freshly re-read evidence set reports `Unclaimed` — no waiting, no window lapse, no cleanup step (`:148-160`).
5. A second clone (`workspaceID + "-a-different-clone"`, unrelated live tokens) derives the **same** evidence and still reports `Held` by the worker — it must age the claim out like any remote (`:164-172`).

`fresh()` in that suite is `claims.Freshness{Now: time.Now(), Window: 24 * time.Hour}` (`internal/app/claims_test.go:87-89`); the read path used is `ListIssues{IncludeArchived:true, IncludeDeleted:true}` + `GetRelationsByIDs` + `ListAllEvents` + `NewEvidence` (`internal/app/claims_test.go:57-85`).
