# Access control: the design

Status: draft, first pass. Answers [charter.md](charter.md); evidence in
[research/competitors.md](research/competitors.md) (cited below as *R§*) and
[research/lit-feature-surface.md](research/lit-feature-surface.md) (cited as
*F§*). Written 2026-08-25. Sections marked **OPEN** are decisions a later
session must close; everything else is a position this draft commits to.

## The shape in one paragraph

Two independent layers over the existing git-synced Dolt store. The **write
layer** makes every mutation a signed Dolt commit and puts a signed policy
document in the store; every client verifies the whole chain on receive and
refuses to build on a commit whose signer lacked the capability for its
mutation — enforcement by every verifier instead of any server, the model
Radicle proves viable over a git substrate (R II) and TUF supplies the role
machinery for (R IV). The **read layer** assigns each issue (and optionally
each comment) a named visibility tier; free-text fields of tiered rows are
encrypted under the tier's current epoch key, and epoch keys are wrapped to
each member's public key in a keyring inside the store — the SOPS recipient
pattern for grants (R IV), the Megolm/Sender-Keys epoch pattern for
revocation (R IV), key destruction for erasure (R III). The layers compose
but neither needs the other: the write layer alone meets the charter's RBAC
bar; the read layer alone meets the confidentiality bar.

Two boundaries are fixed by physics, not by this design, and everything else
is stated in their terms:

- **Forgery is preventable; denial is only recoverable.** No client-side
  scheme can stop a holder of git push access from deleting or overwriting
  `refs/dolt/data`. What it can guarantee is that no such act ever becomes
  *accepted history*: honest clients detect it, refuse it, and re-push from
  any replica. (Monero's miners can censor; they cannot forge.)
- **Read grants are forward-only.** A key once given cannot be taken back for
  content it already covered. Revocation means new epochs for the survivors;
  every studied system — Megolm, Signal sender keys, SOPS, git-crypt — has
  exactly this property (R IV). Consequently there is no cryptographic Deny:
  the policy language is additive grants plus rotation, full stop (R V-f).

## 1. Write layer: signed mutations, verified policy

### Principals

A principal is an Ed25519 keypair. Its public key's fingerprint is the
principal id — opaque, stable, and the only identity form that enters the
synced database. This extends the existing opaque-discriminator invariant
(`design-docs/work-claims.md:284-299`) to actor identity and resolves the
standing contradiction that `assignee`, `actor`, and `created_by` carry
session ids and usernames today (F§3, F§7.10): those columns become principal
ids, and human-readable names live in the keyring entry the principal's key
signs (display name, key, role), rendered at display time. Agent sessions act
*as* a principal: the checkout's stream id keeps identifying *where* work
happened; the principal identifies *on whose authority*. The attribution pair
grows to a triple (stream, workspace, principal) with the same
never-backfilled semantics the pair already has (F§3c). A principal id is
permanent, so key rotation and device replacement need a **succession
record** linking old principal id to new — own-bindings and keyring
entitlements follow the link. Its authorization has two tiers, because a
succession is a takeover of everything the old principal held: when the
old key still exists, its own signature plus one `member.admin`
countersign suffices (the principal consents); when the old key is
claimed lost, the record requires the **owner-set threshold** — the same
quorum as owner rotation — because "their key is gone, reassign it to
this new key" is precisely the single-signer takeover the threshold
exists to stop. A succession for an *owner's* key always requires the
threshold. The record itself moves no key material — epoch keys are wrapped
per member public key (§2), so someone holding the plaintext must actively
re-wrap to the new key. Who does is split the same way: in the consenting
case the old key's holder re-wraps every epoch they hold in the same
transaction; in the lost-key case the threshold-signed record *authorizes*
the transfer, and each tier's restoration is then performed as an ordinary
grant by any current member of that tier — the signing owners need not hold
the keys themselves — with the new principal's access to a tier pending
until some member of it performs the re-wrap. A key lost with no succession issued leaves the old
principal's own-scoped rights stranded until an owner reassigns them
(a documented limit, §3).

Precedent: Radicle's did:key node identities (R II); the keyring-carries-
metadata pattern from SOPS (R IV).

### The policy document

One signed object in the store: the member list (principal → role, plus the
wrapped tier keys per member — §2), the role definitions, and a monotonically
increasing version. Ordinary membership changes are signed by any principal
holding the `member.admin` capability — and "ordinary" is decided by the
verifier, not the signer's framing: **any diff to the set of principals
holding the owner role — add, remove, promote, demote — routes through the
threshold check, no exceptions.** A `member.admin` signature alone covers
only non-owner rows; a policy update that quietly adds a new owner under an
ordinary signature is invalid on its face. The same threshold gates **any
change that widens a role's capability bundle**, because role definitions
live in this same signed document and relabeling is otherwise a complete
bypass: a single owner key could grant "editor" the admin capabilities and
promote an accomplice into it, achieving owner-equivalent control without
the owner set ever changing. A lone `policy.admin` signature therefore
covers only edits that widen nothing — narrowing a bundle, renaming,
reordering; the verifier decides which by diffing the granted surface, not
by trusting the change's framing, exactly as with owner-set diffs. Changes to the owner set follow
TUF root rotation: the new owner list must carry signatures from a
threshold of the old owners **and** the new (R IV). The threshold defaults
to 1 while the owner set has one member — the solo case must stay
frictionless, and a sole owner has no quorum to defend anyway — and to a
**majority of owners** the moment it has more, so in a multi-owner
workspace a single compromised owner key cannot hand the owner set to
itself by default, rather than only after someone opts in. One asymmetry
for the two verbs that strip an owner — *removal* and *demotion*: the
old-owner quorum excludes the owner being removed or demoted — otherwise
majority-of-2 is 2 and a two-owner workspace could never shed, or even
demote, a compromised or unresponsive owner. The symmetric consequence is
stated plainly rather than hidden: in a two-owner workspace either owner
can remove or demote the other, first mover wins; an owner-versus-owner
dispute is a human matter, and every replica retains the pre-removal
history. Setting
the threshold explicitly remains available (the 120% primitive).

### Capabilities and roles

The capability vocabulary is derived from lit's actual command surface (F§1),
because every mutating command is a verb the policy must name. A capability
is a triple: **(verb, selector, binding)**, where binding is the declared
enum **own | assignee | own-or-assignee | all** — verb-split permissions,
own-vs-all splits, and relative principals being the three dimensions that
recur across the market's permission-rich products (R V-a). `own` binds to
the row's creating principal, `assignee` to the issue's current assignee
column (independent of `created_by`, so it works on legacy NULL-owned
rows), and the disjunction is first-class because the market's dynamic
principals are. The selector names the scope (the whole
workspace, for now) and may restrict by issue type — needed because "can't
delete epics" is a type restriction, not an ownership one. For `own`:
comments, labels, and relations already record a creator; issues do not —
issue ownership binds to the creation event's actor, materialized as a first-class `created_by` principal column in the
schema (Required lit redesigns, §2) rather than recomputed from history on
every check. This matches the reporter/author machinery in Jira and GitLab
(R V-d).

Verbs, mapped from the command table: `create`, `edit` (update's field
writes), `start` (claim into in_progress — granted /all, because pulling
the top of the shared queue regardless of creator *is* the agent workflow;
under this layer `start` also records the claiming principal as the issue's
assignee — redesign item 7, §2 — because claims derivation stores nothing
(F§3d) and the `assignee` binding below reads a column, so without this a
claimed ticket would answer to neither `own` nor `assignee`),
`comment.add`, `comment.rm`, `close` (one verb covering both of lit's
close paths — `done` and resolution-close — because both mean "this
ticket's work ends here"), `reopen`, `rank`, `relate`
(dep/parent/label), `archive`/`unarchive`, `delete`/`restore` (tombstone —
sets `deleted_at`, recoverable — and its reversal; a role holds each pair at
one scope, so whoever can delete somewhere can restore there, and likewise
archive/unarchive), `doctor.fix` (doctor --fix's integrity and rank
repairs, which rewrite rows across the backlog), `destroy` (irreversible —
§2 crypto-shred), `tier.set` (re-tiering an *existing* row only; the
initial tier at creation is governed by tier membership — any member may
create into a tier whose keys they hold, as part of `create`),
`member.admin`, `policy.admin`, `sync.take` (the reconcile
take-local/take-remote escape, today guarded only by an advisory token the
client itself computes — F§7.13 — which this layer replaces with a real
owner signature), and `store.replace` (the whole-database replacement
operations: backup restore, snapshots restore, lifeboat recover, downgrade —
one capability because they are one act, replacing history wholesale).
One deliberate exception inside `store.replace`, covering the two
filesystem-level disaster paths — lifeboat recover and snapshots restore:
both exist precisely for stores broken enough that the policy document may
itself be unreadable, and a broken local store cannot cryptographically
gate anything — nor needs to. When no valid policy is readable, the local
surgery proceeds without the check; the result re-enters shared history
only through the receive boundary. And because a mechanically rebuilt
store carries no per-mutation signature chain for peers to verify, a
lifeboat rebuild concludes with a **re-enrollment** — a distinct procedure
from first enrollment, not subject to its no-existing-enrollment fence: a
superseding enrollment commit *names the enrollment it replaces* and is
signed by the owner-set threshold of the regime it supersedes, which is
what peers verify before accepting the rebuilt history. (Total owner-key
loss leaves no cryptographic path — recovery from that is a human
decision among replica holders, documented, not designed around.) Enforcement lives at
acceptance, never at local surgery — which is the write layer's model
everywhere, stated here because this is where it is easiest to get wrong. `lit import` is not its own verb:
it fans out to `create` and `edit` checks per affected issue, so bulk
ingest grants nothing the interactive verbs don't — and the same rule
covers `bulk label/close/archive`, each fanning out to its per-issue verb
(`relate`, `close`, `archive`). Decrypted egress
(`export` and `backup create` emitting plaintext rather than envelopes —
§2's redesign item 3; the lifeboat is not in this list because it has no
decryption logic at all and can only ever dump envelopes) is the named
verb `export.plain`,
granted to every member by default and honest-client only: a keyholder can
always decrypt locally, so this is a guardrail against accidental egress,
not enforcement, and is documented as such. The owner-notify hook — an
arbitrary egress channel by design (F§5 item 9) — sits at the same
honest-client boundary: its event summaries are rendered by the invoking
checkout, so they carry plaintext only for tiers whose keys the invoking
principal holds, and everything else appears in redacted-skeleton form
exactly as in any other render. The hook needs no verb of its own because
it can never emit what its own principal could not already read. The selector is the whole
workspace for now; per-epic delegation is the TUF delegated-paths
primitive (R IV) held in reserve — 120% rule — but not a shipped feature.

Roles are named bundles of capabilities. Four defaults, matching the ladder
R V-a documents for the market's role-based products (ADO's namespace-ACL
model is the structural outlier there; the claim is R V-a's qualified one,
not restated stronger here):

| Role | Capabilities (beyond the previous row) |
|---|---|
| observer | read (holds tier keys granted to them; no write verbs) |
| contributor | `create`, `start`/all, `edit`/own, `comment.add`, `comment.rm`/own, `close`/own-or-assignee (so an agent who claimed a ticket can finish it — the market's author-or-assignee disjunction, R V-d), `delete`+`restore`/own (non-epic) |
| editor | `edit`/all, `close`/all, `reopen`, `rank`, `relate`, `archive`+`unarchive`, `doctor.fix`, `tier.set` |
| owner | everything, including `delete`+`restore`/all, `destroy`, `member.admin`, `policy.admin`, `sync.take`, `store.replace` |

The charter's canonical example — a contributor who can create tickets but
cannot delete epics or others' tickets — is the contributor row: `own` on
`delete` withholds others' tickets, and the non-epic type selector withholds
epics even from their own creator. (The two restrictions are independent,
which is exactly why the capability selector carries an issue-type
dimension.) Note what `delete` means before it can be feared: a tombstone on
a surviving row (F "retention" group), reversible by the paired `restore`.
Irreversible loss is a separate verb (`destroy`), owner-only, mirroring the
soft-delete/destroy split the market enforces to varying depth — true
recycle bins in three products, harder-gated destruction nearly everywhere,
GitLab the cautionary outlier (R V-c's per-product breakdown).

### Enforcement

Every Dolt mutation commit carries the author principal's signature over
(commit content, parent hash, policy version it was authored under).
Verification runs at every receive boundary — `sync fetch/pull`, the
background receive, and first-clone adoption (F§4) — before the data branch
moves: walk the new commits; check each signature; check each mutation
against the policy as of its stated version; and check that each commit's
declared policy version is **at least its parent commit's** — monotonicity
anchored to the parent in the verified chain, so a policy update anywhere
upstream forecloses signing under an earlier, more permissive version
afterward. Anchor it any more loosely (per push, or per signer) and a
demoted principal keeps authoring under the stale version indefinitely,
defeating the revocation the layer exists for. A chain containing an
unauthorized mutation is rejected whole,
the local head stays put, and the client reports (and, holding a valid
replica, re-pushes over) the invalid remote. The **last verified head** is a
new, strictly local value — it lives in the private git dir beside the
stream id, never in any synced table. lit's existing `last_sync_hash` (F§2
meta) shows the bookkeeping precedent but is deliberately *not* reused: that
slot is synced state, so a hostile remote could rewrite it through the very
sync it is supposed to gate — the same defect §2's redesign item 4 removes
`last_sync_path` for. The local anchor is the TUF rollback defense: a remote
serving an older-than-known or diverging history is refused, not adopted
(R IV). Witness cosigning against
split-view (the remote showing different members different histories) is
acknowledged as the full CT-style answer and deliberately deferred; the
primitive slot exists, the feature does not (120/80). To keep charter
constraint 2 intact rather than quietly loosened: a witness is split-view
*detection*, never enforcement, and never required — enforcement is
client-side everywhere, always, and no lit feature may depend on a witness
existing.

**Enrollment — verification never walks to genesis.** Every existing
workspace, this repo's included, has an unsigned history; a rule that
verified from the first commit would reject them all the day it shipped. A
workspace enters the regime at its **enrollment commit** — the commit that
introduces its first policy document, signed by the enrolling owner — which
seals everything before it as adopted by fiat: the enrolling owner vouches
for the pre-enrollment history exactly as `init`'s remote-adopt vouches for
a cloned backlog today. Verification and the last-verified head both seed at
enrollment; a fresh workspace's enrollment commit is simply its first. Only
history after enrollment carries the write layer's guarantees, and the
boundary is inspectable — an auditor sees precisely where fiat ends and
proof begins. First enrollment is an explicit command and it is **sync-fenced**:
it requires a fresh fetch showing the remote holds no enrollment, and it
pushes immediately — so on a shared remote there is one winner by ordinary
push ordering. (The fence governs *first* enrollment only; a superseding
re-enrollment after disaster recovery is the distinct threshold-signed
procedure described in §1's store.replace exception.) A race that slips through anyway (two unsynced clones each
enrolling) is caught at reconcile: two distinct enrollment commits never
auto-merge; the one already in the remote's linear history stands, the
losing clone discards its enrollment and its principal re-requests
membership. The exact fencing mechanics are §6's to close.

Reconcile is compatible with signing because it already replays folded local
commits one-per-original with original authorship (F§4) — each replayed
commit is re-signed by the reconciling principal with a link to the original
signature. Replay also crosses policy versions — a commit validly authored
under policy v1 gets reparented onto a head that may already be at v2 — so
the rule is split: for **monotonicity**, a replayed commit re-declares its
new parent's policy version; for **authorization**, it is judged against
the version of its original authorship, because judging a v1 mutation
against v2's roles would retroactively rewrite what its signer was allowed
to do at the time. That original version is never taken on the
reconciler's say-so: the original commit's own signature already covers
the version it was authored under, so every verifier follows the replay
link and re-checks the claimed version against the original signed
payload itself. The reconciler's signature attests to one thing only —
faithful replay — and a compromised reconciler that declares an earlier,
more permissive version than the original signature carries is rejected
like any other forgery. **OPEN:** the exact signed payload for replayed
commits, so an auditor can distinguish "authored" from "faithfully
replayed" without trusting the reconciler.

What the write layer deliberately does not do: prevent a pusher from
destroying the remote ref (recoverable, per the fixed boundary above), or
police reads (that is the read layer's job — a signature never hid anything).

## 2. Read layer: tiers, epochs, wrapped keys

### The granularity decision

The unit of *policy* is the tier label on a row; the unit of *encryption* is
the field value. Concretely:

- Every issue carries a `tier` column. The tier governs the issue's free-text
  fields: `title`, `description`, `agent_prompt`, `topic` (an ordinary
  encryptable column once the id redesign below stops embedding it), and the
  issue's `issue_events.reason` and `issue_event_changes` values (which
  duplicate free text in plaintext today — F§2).
- Every comment carries its own `tier` column, defaulting to its issue's.
  So the charter's question — can a single comment be encrypted? — has a
  clean answer: **yes, because comments are rows, not a field** (F§2), and
  per-comment restriction is exactly the market's demand ceiling: Jira
  restricted comments, GitLab internal notes (R V-b). Nothing finer exists
  anywhere, and nothing finer is designed here.
- Structural columns — ids, status, type, rank, lane, timestamps, relations,
  labels, resolution — stay plaintext. **A tier hides content, not
  existence.** Every workspace member sees that a ticket exists, where it
  ranks, and how it relates; only tier members read what it says.

The visible-skeleton rule is the load-bearing simplification, and it is what
keeps the strain points from becoming redesign-everything: claims derivation
reads only structural columns and whole-row-set evidence, so it keeps working
unchanged (F§7.1); rank stays one global order (F§7.4); lane gating and
epic-status derivation keep reading full child sets (F§7.6); the backlog
lists every ticket and redacts titles the caller can't decrypt (F§7.5).
Existence-hiding would break all four at once and is explicitly out of
scope — a workspace whose ticket *existence* is secret from some members is
two workspaces, and the one-repo constraint (charter #1) already tells us
that boundary is the repo itself.

Ciphertext envelope: `{tier, epoch, nonce, ciphertext, commitment}` stored
in the existing text columns. The nonce is fresh per encryption. The
**encryption key is the row's content key** — the independent per-row
randomness the destroy scheme below requires, stored wrapped under the
tier's epoch key named by `epoch`; the wrapped copy rides the row (one slot
per issue and per comment), not each envelope, so the envelope format needs
no key slot of its own. It is never an HKDF derivation of the epoch key —
the destroy scheme below says why. HKDF derivation from the epoch key is
restricted to one job: the **commitment** — a deterministic keyed hash of
the plaintext, its key a domain-separated subkey of **the epoch key in
force at write time** — present so that equality of content is
decidable without decryption — the merge below depends on it. Keying the
commitment to the epoch is a deliberate choice between two defects: a
stable per-tier commitment key would survive rotation but hand every
removed member a permanent oracle for testing plaintext guesses against
*future* content, while an epoch-keyed commitment merely loses equality
detection *across* a rotation — so a concurrent edit pair straddling a
membership change can surface a spurious prose conflict, rare (rotations
happen on membership change) and safe (a keyholder confirms the texts
match). Confidentiality wins over a rare liveness annoyance. The remaining
cost: anyone can see that two same-epoch encrypted values are equal (never
what they are) — the same trade git-crypt makes for diffability (R IV),
paid here only at the granularity of whole field values. The envelope is opaque bytes to the schema, to
migrations, to the export format, and to Dolt merge — which is what makes
the sync story below hold.

### Keys, grants, revocation, erasure

Per tier, a symmetric **epoch key**. Every epoch's key — current and
historical — is wrapped to each entitled member's public key in the keyring
(SOPS recipient pattern, R IV). Granting read wraps **all of the tier's
epoch keys** to the new principal, so a joiner reads the tier's whole
existing backlog, which is what the onboarding flow (§5) promises; revocation
is asymmetric by nature — leavers keep old epochs, joiners get them — and
only the *future* is partitioned by rotation. This is the Monero view-key
separation: read capability granted without any write capability. The
converse also holds here — a CI principal can hold write verbs and zero
tier keys — though that half is this design's own key-type independence,
not a Monero precedent (Monero's spend side needs the view key in
practice).
Removing a member rotates the tier to a new epoch wrapped to survivors only
(Megolm / Sender-Keys, R IV); old epochs are not re-encrypted, and the
removed member's continued ability to read what they could already read is
documented, not hidden. `destroy` = crypto-shredding **content keys**, and a
content key must be **independent randomness generated at the row's
creation, stored wrapped under the row's tier's epoch key**, never an HKDF
derivation from the epoch key: a derived subkey is a deterministic public
function of a secret every tier member still holds, so "destroying" it
destroys nothing — any member recomputes it on demand. The unit is the
row, because comments tier independently of their issue (§2 granularity): a
comment encrypted under its *issue's* key would be readable by issue-tier
members whatever the comment's own tier said, so **each encrypted row — the
issue and every one of its comments — carries its own content key**, and
destroying an issue shreds the issue's key *and all of its comments' keys*,
whatever tiers those comments sit in. Nothing of a destroyed issue's
content survives in recoverable form; `destroy` deletes the wrapped copies
from live state and no holder of any epoch key can reconstruct them. Two
honest residuals. First, the substrate is append-only: superseded wrapped
copies persist in synced *history* until history compaction makes them
unreachable — so a shred is complete only when that closes, which is named
as part of §6's keyring/history item. Second, the envelope's commitment
outlives the shred and its key is an epoch subkey members still hold, so a
tier member can test a *guessed* plaintext against a destroyed field
forever — destroy removes recovery, not confirmation of a guess. This is the GDPR Art. 17 and ITAR §120.54 story
(R III), stated with its actual boundary.

Every workspace has a default tier that all members hold. **The default
posture for a new workspace is: one tier, encrypted, all members entitled.**
That single decision is what separates "repo reader" from "workspace member"
by default — the charter's adoption complaint — while costing a member
nothing: they hold the key, everything decrypts, lit looks exactly as it
does today. A project that wants its backlog world-readable (lit's own repo
does) sets the default tier to plaintext; that is the documented public mode,
not the default.

### Sync, merge, and the tier-blind client

The rule that makes the whole read layer survive lit's sync machinery:
**ciphertext is the synced representation.** Encryption and decryption happen
at the CLI/render boundary, never in the store, export, or merge layers.
Consequences, mapped to the strain points:

- The export (`model.Export`) carries envelopes verbatim, so a client
  lacking a tier key still produces a *complete* export, and `diffExports`
  never mistakes unreadable for absent — the silent-deletion failure mode of
  F§7.3 and the cascade cycle of F§7.7 cannot fire.
- Field-aware merge compares envelope **commitments**, not envelope bytes —
  fresh nonces make byte comparison see divergence in two independent
  writes of the *same* text (two agents fixing the same typo would conflict
  forever). Equal commitments merge silently as today; a genuinely
  both-sides-divergent encrypted field is `ProsePending` *for keyholders*:
  the pending fingerprint is computed over the envelopes
  (redefining F§7.2's plaintext fingerprint), and the bijection rule scopes
  to the tiers the reconciling principal holds. A checkout that cannot read
  the conflicted tier cannot reconcile it — it reports which tier is needed
  and leaves the divergence for a keyholder. This narrows today's "any
  checkout can reconcile anything" and is accepted: an agent that can't read
  a ticket has no business merging its prose.
- `--search` and any other SQL-over-plaintext predicate (F§7.5) move to
  client-side filtering after decryption for encrypted fields. Slower, and
  correct; searchable encryption is a wheel this design refuses to invent
  (charter #5).

### Required lit redesigns

Charter rule 4 applied — places where the current model, not the security
model, yields:

1. **Issue-id scheme.** Ids embed the plaintext topic slug and a hash
   commitment to title+description (F§2). Ids become opaque
   (`<prefix>-<base36>`); `topic` becomes an ordinary column, encryptable
   like other free text. Child `.n` suffixes may stay — structure is visible
   by rule.
2. **Actor identity columns** become principal ids with keyring-resolved
   display (F§7.10); `--assignee` filtering and `lit orphaned` operate on
   principal ids. Issues additionally gain a first-class `created_by`
   principal column — the schema has none today, and the write layer's
   "own" binding needs it as a column rather than a per-check replay of the
   creation event. Backfill rule for pre-existing issues: there is no
   truthful source (the id hash's creator input is a fixed placeholder —
   see the feature inventory), so legacy issues get `created_by = NULL`;
   NULL-owned rows answer only to /all capabilities for own-bindings —
   the `assignee` binding reads the assignee column, not `created_by`, so
   it works on NULL-owned rows unchanged — and an owner may explicitly
   assign ownership. For the motivating solo-owner backlog this
   is lossless; for a team migrating an existing shared backlog it is a
   documented one-time cost, not a silent one.
3. **Plaintext-egress commands become a capability.** `export`,
   `backup create`, and `lifeboat dump` today treat a full plaintext dump as
   an ordinary read (F§5). Under the read layer they emit envelopes by
   default; emitting *decrypted* content is a distinct flag gated like any
   other verb. The lifeboat keeps its schema-agnostic below-the-gate
   design untouched (F§7.9) precisely *because* what it dumps is envelopes —
   the store's at-rest form is already the protected form.
4. **`meta.last_sync_path`** — a local filesystem path in the synced DB
   (F§5.11) — is removed independently of everything else; it violates the
   existing privacy invariant today.
5. **Recovery verification** (`verify.go`) loses free-text mis-map detection
   under uniform ciphertext (F§7.8); conservation laws extend to envelope
   integrity (tier/epoch/nonce/commitment round-trip — the commitment
   especially, being the one field derived from content and so the one
   that can catch a scrambled-field mis-map) to compensate.
6. **Foreign-store opens get the same gate as local ones.** `ls --at` and
   `stores --counts` open other projects' stores by path with no policy or
   key consultation (F§7.11). They route through the target store's own
   policy and keyring exactly as a local open does: verification applies,
   the caller's principal supplies whatever tier keys it holds *for that
   store*, and a principal with none sees the skeleton only. No separate
   trust path exists for "but I opened it by filesystem path."
7. **`start` sets the assignee.** Today `assignee` is written only by
   `update` (F§1), and claims derivation deliberately stores nothing
   (F§3d) — so a contributor who claimed a ticket they didn't create would
   be neither its owner nor its assignee and could never `close` it. Under
   the write layer, `start` additionally records the claiming principal in
   the issue's `assignee` column. A declared lit behavior change beyond
   what F§3d documents, and a natural one: the market's assignment
   semantics are exactly "the person working it" (R V-d).

## 3. Foundational hard requirements

From the compliance analysis (R III, R V-f), split as the charter demands.

**Cannot be disabled** (present in every workspace, including solo plaintext
ones — these are what make the audit trail trustworthy at all):

- Unique cryptographic principal per actor (HIPAA 164.312(a) unique user
  identification, Required).
- Signed mutations and receive-time verification (NIST AC-3: enforcement
  must be technical, not procedural).
- The append-only signed history itself — Dolt's commit-per-mutation chain
  under signatures is the AU-9-protected audit log, and a stronger one than
  the incumbents' (GitHub: 180 days, no issue-create events; GitLab: no
  issue events at all — R I).
- Policy-version monotonicity and last-verified-head rollback refusal.
- The tombstone-before-destroy ordering: no verb erases content that a lower
  verb couldn't recover, except `destroy`, which is owner-gated and logged.

**Can be configured or disabled:**

- Encryption itself (a public project runs a plaintext default tier).
- The role set and every capability binding (the four roles are defaults,
  not law).
- Owner-set threshold (defaults to 1 solo, majority once multi-owner).
- Witness cosigning, when it ships (detection only — enforcement never
  depends on it, per charter constraint 2).

**Documented limits, never papered over:** forward-only revocation; denial
recoverable-not-preventable; a removed member retains what they could read;
metadata (existence, structure, timing, actor principal ids) is visible to
every repo reader even under full encryption, as is equality of two
encrypted values (the commitment trade, §2); narrowing an issue's tier
restricts only its future — every prior member of the old, broader tier
keeps the keys to its pre-narrowing history (§6 item 7); widening an
issue's tier grants only its present — the destination tier's members read
the issue's current field values, while its pre-widening history stays
under the old tier's keys (§6 item 7 again, the same forward-only coin
from the other side); and a member who
loses a signing key without a succession record (§1) leaves their
own-scoped rights attached to the old principal until an owner reassigns
them. Compliance officers get these
in writing; the design's honesty here is a feature (R III's regimes all
demand accurate control descriptions, not perfect controls).

## 4. Forward constraints on lit's evolution

The charter asks not for feature prediction but for the boundaries this
design imposes. A future lit change stays compatible iff:

1. **New free-text columns go through the envelope.** Any column that can
   carry user prose is ciphertext-capable from birth.
2. **New mutating commands declare a verb.** The policy vocabulary grows
   with the command surface; a command without a capability mapping cannot
   ship.
3. **Shared structure may not depend on another tier's plaintext.** Rank,
   lanes, claims, epic status — anything computed across rows reads
   structural columns only. This is true today (F§7.1, F§7.4, F§7.6) and becomes a
   law: a feature needing cross-tier *content* is misdesigned.
4. **The synced representation is the protected representation.** No layer
   below the CLI boundary ever sees plaintext; escape hatches inherit
   protection for free. (This is why the lifeboat needed no exemption.)
5. **No feature may require a server.** Anything needing central state is
   out — or it is a witness: an optional external observer that detects
   equivocation and enforces nothing, which is why it does not breach the
   charter's no-server constraint. Enforcement never moves off the client.
6. **Structural columns are workspace-public by definition**, so adding a
   structural column is adding metadata every repo reader can see; that
   trade-off gets named in the adding change's design, every time.
7. **Detached processes get non-interactive key access.** The on-change
   mirror and background receive (F§7.12) already run unattended; key
   material lives where the stream id lives (private git dir / OS keychain),
   never behind a prompt. A future feature that would need an interactive
   unlock is misdesigned.

The known future directions survive these rules: a Jira bridge
(`project-intent.md`) is a principal with an explicit role and tier grants;
friendly stream labels (`work-claims.md` open edge) become keyring metadata
rather than synced plaintext.

## 5. User-facing shape (broad strokes)

The transparency bar is Monero's: correct use requires zero cryptographic
knowledge. What that means concretely:

- **Solo user:** `lit init` mints the owner principal silently (as the
  stream id is minted today — F§3a) and prints one thing: a recovery phrase,
  with one sentence saying what losing it costs. Every command thereafter
  looks exactly like today. This is the entire solo experience.
- **Adding someone:** the joiner runs `lit init` on their clone; it detects
  the protected store, pushes a signed membership request, and says "ask the
  owner to approve." The owner runs `lit member add <name>` and picks a
  role. Key exchange, wrapping, and epoch handling are invisible.
- **Restricting something:** `lit new --tier internal` or
  `lit update <id> --tier internal`; `lit comment add --tier internal`.
  Members of the tier see nothing different; others see the skeleton with
  redacted text.
- **Removing someone:** `lit member rm <name>` — rotation happens; the
  output states plainly that they keep what they could already read.

Burdens, honestly: one recovery phrase to store; an approval step when
someone joins; reconcile of restricted tickets only on machines holding the
tier. Risks, honestly: **key loss is the data loss** — mitigated by every
tier key being held by all its members (any one survivor re-wraps for a
recovered owner) and by the recovery phrase; the genuinely fatal case is a
sole member losing both key and phrase, which is the same fatality as losing
a password-manager vault, and is what the printed phrase exists to prevent.
Rollback or vandalism of the remote surfaces as a loud refusal with a
one-command repair (re-push from any replica), not as silent divergence.

## 6. What the next sessions must close

1. The replayed-commit signature payload (§1, OPEN).
2. `destroy` completion on an append-only substrate: per-row content keys
   are decided (random at row creation, wrapped under the row's tier epoch —
   §2); what remains open is making superseded wrapped-key material
   *unreachable in synced history*, without which a shred is incomplete.
   Couples to item 4.
3. Crypto suite selection and the FIPS question (ITAR wants validated
   modules — R III; Go's stdlib vs a vetted library; no home-rolled
   primitives under charter #5).
4. Keyring/policy wire format, growth, and where it lives in the store —
   the keyring is O(members × epochs-ever-minted) with wrapped keys for
   every historical epoch, synced to every clone, so the format decision
   must state a bound or a compaction strategy (or argue concretely why
   churn keeps it small); also (a table vs a
   Dolt-versioned file; it must ride reconcile safely).
5. The joiner-request flow's abuse surface (an unauthenticated ref push as a
   membership request is a spam channel; probably fine, needs a look).
6. Verification cost at receive time on long histories (adoption clones the
   full archive — F§4; incremental verification from last-verified-head
   should make steady-state cheap; the first adopt is the expensive one).
7. `tier.set` mechanics in both directions. Position: **narrowing** is
   future-only — existing history stays under the old tier, stated plainly,
   consistent with forward-only everything. **Widening** mints a fresh
   content key for the row, re-encrypts the issue's *current* field values
   under it, and wraps it to the destination tier's epoch key (one issue's
   worth of work, cheap), because "make this visible to more people" that
   leaves the content unreadable to them is a lie; the old content key and
   the *history* it encrypts stay under the old tier — re-wrapping the
   existing key would expose the history too — and this is documented as
   such (§3's limits list). The open
   detail is the widening rewrite's interaction with sync (it is an
   ordinary signed mutation, but the re-encryption must be performed by a
   principal holding both tiers).
8. The enrollment fence made precise (§1): the fetch-check-push window, and
   reconcile's detection and discard of a losing concurrent enrollment.
