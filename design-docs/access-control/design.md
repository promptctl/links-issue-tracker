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
never-backfilled semantics the pair already has (F§3c).

Precedent: Radicle's did:key node identities (R II); the keyring-carries-
metadata pattern from Keybase and SOPS (R IV).

### The policy document

One signed object in the store: the member list (principal → role, plus the
wrapped tier keys per member — §2), the role definitions, and a monotonically
increasing version. Ordinary membership changes are signed by any principal
holding the member-admin capability. Changes to the *owner set itself* follow
TUF root rotation: the new owner list must carry signatures from a threshold
of the old owners **and** the new (R IV) — so a compromised single owner key
cannot silently hand the workspace to itself. The threshold defaults to 1
(the solo-owner case must stay frictionless); raising it is the 120%
primitive already present.

### Capabilities and roles

The capability vocabulary is derived from lit's actual command surface (F§1),
because every mutating command is a verb the policy must name. A capability
is a triple: **(verb, scope, own|all)** — the three dimensions the entire
market converges on: verb-split permissions, own-vs-all splits, and relative
principals (R V-a). "Own" binds to the row's creating principal
(`created_by`), matching the reporter/author machinery in Jira and GitLab
(R V-d).

Verbs, mapped from the command table: `create`, `edit` (update's field
writes), `comment.add`, `comment.rm`, `close`, `reopen`, `rank`, `relate`
(dep/parent/label), `archive`, `delete` (tombstone — sets `deleted_at`,
recoverable), `destroy` (irreversible — §2 crypto-shred), `tier.set`,
`member.admin`, `policy.admin`, `sync.take` (the reconcile
take-local/take-remote escape, today guarded only by an advisory token the
client itself computes — F§7.13 — which this layer replaces with a real
owner signature). Scope is the workspace for now; per-epic scope is the TUF
delegated-paths primitive (R IV) held in reserve — 120% rule — but not a
shipped feature.

Roles are named bundles of capabilities. Four defaults, matching the ladder
every product converges on (R V-a):

| Role | Capabilities (beyond the previous row) |
|---|---|
| observer | read (holds tier keys granted to them; no write verbs) |
| contributor | `create`, `edit`/own, `comment.add`, `comment.rm`/own, `close`/own, `delete`/own |
| editor | `edit`/all, `close`/all, `reopen`, `rank`, `relate`, `archive`, `tier.set` |
| owner | everything, including `delete`/all, `destroy`, `member.admin`, `policy.admin`, `sync.take` |

The charter's canonical example — a contributor who can create tickets but
cannot delete epics or others' tickets — is the contributor row verbatim.
Note what `delete` means before it can be feared: a tombstone on a surviving
row (F "retention" group), reversible by `restore`. Irreversible loss is a
separate verb (`destroy`), owner-only, mirroring the soft-delete/destroy
split five of six incumbents enforce and GitLab is the cautionary outlier for
(R V-c).

### Enforcement

Every Dolt mutation commit carries the author principal's signature over
(commit content, parent hash, policy version it was authored under).
Verification runs at every receive boundary — `sync fetch/pull`, the
background receive, and first-clone adoption (F§4) — before the data branch
moves: walk the new commits; check each signature; check each mutation
against the policy as of its stated version; check policy-version
monotonicity. A chain containing an unauthorized mutation is rejected whole,
the local head stays put, and the client reports (and, holding a valid
replica, re-pushes over) the invalid remote. lit already persists
`last_sync_hash` (F§2 meta) — that slot becomes the **last verified head**,
which is the TUF rollback defense: a remote serving an older-than-known or
diverging history is refused, not adopted (R IV). Witness cosigning against
split-view (the remote showing different members different histories) is
acknowledged as the full CT-style answer and deliberately deferred; the
primitive slot exists, the feature does not (120/80).

Reconcile is compatible with signing because it already replays folded local
commits one-per-original with original authorship (F§4) — each replayed
commit is re-signed by the reconciling principal with a link to the original
signature. **OPEN:** the exact signed payload for replayed commits, so an
auditor can distinguish "authored" from "faithfully replayed" without
trusting the reconciler.

What the write layer deliberately does not do: prevent a pusher from
destroying the remote ref (recoverable, per the fixed boundary above), or
police reads (that is the read layer's job — a signature never hid anything).

## 2. Read layer: tiers, epochs, wrapped keys

### The granularity decision

The unit of *policy* is the tier label on a row; the unit of *encryption* is
the field value. Concretely:

- Every issue carries a `tier` column. The tier governs the issue's free-text
  fields: `title`, `description`, `agent_prompt`, and the issue's
  `issue_events.reason` and `issue_event_changes` values (which duplicate
  free text in plaintext today — F§2).
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

Ciphertext envelope: `{tier, epoch, nonce, ciphertext}` stored in the
existing text columns. The envelope is opaque bytes to the schema, to
migrations, to the export format, and to Dolt merge — which is what makes the
sync story below hold.

### Keys, grants, revocation, erasure

Per tier, a symmetric **epoch key**. The current epoch's key is wrapped to
each entitled member's public key in the keyring (SOPS recipient pattern,
R IV). Granting read = wrapping the key to one more principal — the Monero
view-key separation: read capability granted without any write capability,
and vice versa (a CI principal can hold write verbs and zero tier keys).
Removing a member rotates the tier to a new epoch wrapped to survivors only
(Megolm / Sender-Keys, R IV); old epochs are not re-encrypted, and the
removed member's continued ability to read what they could already read is
documented, not hidden. `destroy` = delete a per-issue derived key... **OPEN:**
whether `destroy` shreds at tier-epoch granularity (simple, coarse: destroys
every same-tier-same-epoch value) or introduces per-issue subkeys derived
from the epoch key (HKDF per issue id — still one wrapped secret per member,
but shreddable per issue). The per-issue derivation is the current lean: it
costs no keyring growth and makes crypto-shredding — the GDPR Art. 17 and
ITAR §120.54 story (R III) — precise enough to erase one ticket.

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
- Field-aware merge compares envelope bytes. Single-side change wins as
  today; a both-sides-divergent encrypted field is `ProsePending` *for
  keyholders*: the pending fingerprint is computed over ciphertext
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
   principal ids.
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
   integrity (tier/epoch/nonce round-trip) to compensate.

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
- Owner-set threshold (defaults to 1).
- Witness cosigning, when it ships.

**Documented limits, never papered over:** forward-only revocation; denial
recoverable-not-preventable; a removed member retains what they could read;
metadata (existence, structure, timing, actor principal ids) is visible to
every repo reader even under full encryption. Compliance officers get these
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
   structural columns only. This is true today (F§7.1, F§7.4) and becomes a
   law: a feature needing cross-tier *content* is misdesigned.
4. **The synced representation is the protected representation.** No layer
   below the CLI boundary ever sees plaintext; escape hatches inherit
   protection for free. (This is why the lifeboat needed no exemption.)
5. **No feature may require a server.** Anything needing central state is
   out — or it is a witness, which is optional by definition.
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
2. Per-issue key derivation for `destroy` (§2, OPEN — leaning HKDF-per-issue).
3. Crypto suite selection and the FIPS question (ITAR wants validated
   modules — R III; Go's stdlib vs a vetted library; no home-rolled
   primitives under charter #5).
4. Keyring/policy wire format and where it lives in the store (a table vs a
   Dolt-versioned file; it must ride reconcile safely).
5. The joiner-request flow's abuse surface (an unauthenticated ref push as a
   membership request is a spam channel; probably fine, needs a look).
6. Verification cost at receive time on long histories (adoption clones the
   full archive — F§4; incremental verification from last-verified-head
   should make steady-state cheap; the first adopt is the expensive one).
7. Whether `tier.set` to a *more* restricted tier should re-encrypt the
   issue's existing history or only future writes (leaning: future-only,
   stated plainly, consistent with forward-only everything).
