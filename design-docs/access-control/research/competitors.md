# Research: RBAC and data-visibility models in issue trackers

Evidence base for [../design.md](../design.md). Compiled 2026-08-25 by a
research agent from vendor and primary documentation; claims verified against
sources on that date unless flagged UNVERIFIED. This is a findings record, not
a design position — the design doc decides what to adopt.

## Part I — Product systems

### 1. GitHub Issues

**Roles.** Five predefined repo roles — Read, Triage, Write, Maintain, Admin —
plus org owner ([repository-roles-for-an-organization](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/repository-roles-for-an-organization)).
Read: view/open/comment. Triage adds: close/reopen/assign all issues, labels,
milestones, mark duplicates, "hide anyone's comments." Write adds: lock
conversations, transfer issues, delete comments. Admin adds: "edit and delete
anyone's comments," delete issues.

**Delete semantics.** Issue deletion is *off by default org-wide*; an org owner
must enable it, after which only admins/owners can delete
([allowing-people-to-delete-issues](https://docs.github.com/en/organizations/managing-organization-settings/allowing-people-to-delete-issues-in-your-organization)).
Deletion is a hard delete, but admins retain deletion metadata and the event is
audit-logged (`issue.destroy`). Comments have a two-tier model: **"minimize"**
(Triage+, reason-tagged soft tombstone — any reader can still expand it) vs
**delete** (Write+ — or the comment's own author regardless of role, GitHub's
own/all split for comments; hard delete of content but a timeline event
remains visible to all readers, and comment *edit history* is retained). **Lock conversation**
(Write+): after locking, only write-access users can comment.

**Read granularity: repository-level only.** No per-issue confidentiality, no
per-comment read restriction. The standard workaround is a second private repo
(verified by absence across permissions docs).

**Write granularity.** Repo roles, refined by **custom repository roles**
(Enterprise Cloud only) with issue-specific grants: "Close an issue", "Reopen a
closed issue", "Delete an issue", "Mark an issue as a duplicate"
([about-custom-repository-roles](https://docs.github.com/en/enterprise-cloud@latest/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/about-custom-repository-roles))
— so delete can be delegated below Admin on Enterprise.

**Creator semantics.** "Anyone can close an issue they opened" without any
role; authors edit their own issue/comments. UNVERIFIED: whether an author can
reopen after a maintainer closes (community discussions suggest often not; docs
state no clean rule). Authors can never delete their own issue.

**Audit log.** Org audit log, 180 days, owner-only. Issue events:
`issue.destroy/pinned/unpinned/transfer`, `issue_comment.destroy/update`. Issue
create/close are *not* logged — destruction and mutation of others' content
are. UNVERIFIED: exact plan gating of the audit-log UI.

**Crypto/client-side enforcement: none** (confirmed by absence).

### 2. Jira (Atlassian Cloud)

Terminology note: current docs say "spaces"/"work items" for projects/issues;
permission names are unchanged in substance.

**Permission schemes** (company-managed projects only; not on Free). Verified
permission list includes: Browse Projects, Administer Projects,
Create/Edit/**Delete work items**, Assign, Assignable user, Close, Resolve,
Transition, Schedule, Move, Link, Modify reporters, **Archive work items** +
Restore archived work items, **Set work item security**
([work-item-permissions](https://support.atlassian.com/jira-cloud-administration/docs/work-item-permissions/)).
Comments/attachments/worklogs each have a confirmed **Own vs All split**: "Edit
own comments" / "Edit all comments" / "Delete own comments" / "Delete all
comments" (same for attachments and worklogs). No "Edit Own Issues" — the
Reporter dynamic grant is the workaround.

**Grant targets** (permission-scheme holders): user, group, **project role**,
application access, anyone/public, **reporter**, **current assignee**, project
lead, user/group custom-field value. Granting to Reporter is direct "creator
owns their ticket" machinery. UNVERIFIED: a KB caveat that granting Browse to
reporter/assignee/custom-field leaks project metadata to all users — the
specific KB article was not re-located for a link.

**Read granularity — the market high-water mark, three nested levels:**

1. Project: Browse Projects.
2. Issue: **issue security schemes** — named security levels per issue;
   non-members "won't be able to view those work items, even if they can access
   the rest of the space"
   ([configure-issue-security-schemes](https://support.atlassian.com/jira-cloud-administration/docs/configure-issue-security-schemes/)).
   Level members can be users, groups, project roles, **Reporter**, **Current
   assignee**, and multi-user/group picker custom fields (dynamic per-issue
   membership). "Set work item security" governs who changes an item's level.
3. Comment: **restricted comments** (padlock) — restrict a comment to a project
   role or group you belong to
   ([KB](https://support.atlassian.com/jira/kb/add-restricted-comments-on-jira/)).
   JSM adds role/group-restricted internal notes.

**Write granularity.** Per-verb carve-outs (Assign, Modify reporters, Resolve,
Transition, Schedule are separate from Edit), plus the Own/All splits above.

**Team-managed projects** have no schemes and no issue security: access levels
Open/Limited/Private with fixed Administrator/Member/Viewer roles.

**Defaults.** Default scheme grants broadly to "any logged-in user";
team-managed default is Open; on Free everyone effectively has project admin.
Issue security is opt-in — no scheme means every browser sees every issue.

**Compliance.** **BYOK encryption** (customer AWS KMS keys, Cloud Enterprise
only, now closed to new customers in favor of **Customer-Managed Keys, CMK**) —
this is at-rest key custody only; Atlassian still decrypts server-side to
enforce permissions, so it is *not* cryptographic access control
([BYOK doc](https://support.atlassian.com/security-and-access-policies/docs/what-is-byok-encryption-for-atlassian-products/)).
Data residency: Standard+. HIPAA BAA: Standard/Premium/Enterprise. FedRAMP:
Atlassian Government Cloud is the FedRAMP-scoped offering
([atlassian.com/government](https://www.atlassian.com/government));
commercial cloud not covered. UNVERIFIED: the specific claim "FedRAMP
Moderate achieved March 2025" was not confirmed against a primary source. Org audit log; enhanced via Atlassian Guard. **No
client-side enforcement — confirmed.**

### 3. Broadcom Rally (ex-CA Agile Central)

**Hierarchy/roles.** Subscription → Workspaces → nested Project tree.
Workspace-level: No Access, User, Workspace Admin. Project-level: **No Access
("default value for each user")**, **Viewer** (read-only, all items in
project), **Editor** ("create, edit, and delete all work items inside the
project"), **Team Member** (Editor + appears in owner dropdowns), **Project
Admin** (Editor + manage lower-role permissions; cannot delete users)
([Broadcom techdocs](https://techdocs.broadcom.com/us/en/ca-enterprise-software/valueops/rally/rally-help/administration/managing-users/creating-and-editing-users/set-user-access-permissions.html)).

**Delete semantics.** "Users that are **owners of a work item** or that have
Project Editor privileges can delete" — owner-may-delete-own-item is a direct
creator-ownership mechanism. Delete goes to a **Recycle Bin**; restore requires
Editor+; **permanent delete from the Recycle Bin is restricted to
subscription/workspace administrators**. UNVERIFIED: Recycle Bin scope
(per-project vs per-workspace); whether permission grants offer automatic
flow-down to child projects (docs only document copy-users-at-creation and
workspace defaults, no live inheritance).

**Granularity.** Finest read = project; finest write = project. No per-item
security levels, no per-comment restriction. **No crypto enforcement —
confirmed absent.**

### 4. GitLab issues/epics

**Roles** (current): Minimal Access, Guest, **Planner** (added 17.7 — a
dedicated issues/epics/milestones role between Guest and Reporter), Reporter,
Security Manager, Developer, Maintainer, Owner
([permissions](https://docs.gitlab.com/user/permissions/)). Membership
per-project or per-group (cascades).

**Delete semantics.** The "Delete issue" permission row footnote: **"Users who
don't have the Planner or Owner role can only delete the issues they
authored"** — so Planner/Owner delete anything; Reporter/Developer/Maintainer
self-delete only. Hard delete, no tombstone, and the
[audit event types list](https://docs.gitlab.com/user/compliance/audit_event_types/)
contains **no issue-level events** (it does have `delete_epic`) — the weakest
forensic story among the four products whose audit trails this document
records (GitHub, Jira, Linear, GitLab; Rally's and ADO's were not surveyed). Deleting an epic detaches (doesn't delete)
its issues. Close/reopen/edit: role Planner+ **or author or assignee**.

**Read granularity.** Per-issue: **confidential issues** — visible to
Planner/Reporter/Developer/Maintainer/Owner; Guest authors see only their own;
Guests/non-members gain access via *assignment*, revoked on unassignment
([confidential issues](https://docs.gitlab.com/user/project/issues/confidential_issues/)).
**Confidential epics** enforce transitively: "a confidential epic can only
contain confidential issues and confidential child epics." Per-comment:
**internal notes** — Reporter+ to view; one-way ("can't convert internal notes
to regular comments"; replies inherit internal status). Granularity is
per-comment, but against *fixed role thresholds*, never arbitrary principals —
the only principal-level grant is the confidential-issue assignee carve-out.

**Creator semantics — strongest of the incumbents.** The "author-or-assignee
OR role" disjunction appears in most issue mutation prerequisites (close,
reopen, edit, promote); self-delete is narrower — authors only, per the
footnote quoted above, with no assignee carve-out.

**Custom roles**: Ultimate tier. Audit events: Premium/Ultimate, retained
indefinitely. **No crypto enforcement — confirmed.**

### 5. Azure DevOps Boards

**Model.** All ACLs live in **security namespaces** with per-identity
Allow/Deny bitmasks. Area-path work-item permissions are in the `CSS` namespace
(`WORK_ITEM_READ`, `WORK_ITEM_WRITE`, …) on hierarchical tokens; UI names:
**"View work items in this node"**, **"Edit work items in this node"**, "Edit
work item comments in this node" (Services only). Project-level (not
per-area): "Delete and restore work items", **"Permanently delete work
items"**, "Move work items out of this project", "Change work item type"
([set-permissions-access-work-tracking](https://learn.microsoft.com/en-us/azure/devops/organizations/security/set-permissions-access-work-tracking)).
Iteration paths have no view/edit work-item actions — not a visibility
boundary.

**Semantics.** Deny overrides Allow, except explicit child-node Allow beats
inherited parent Deny; "Not set" = implicit deny. Even Project Collection
Administrators cannot bypass a work-item delete Deny. **Finest read
restriction = area path; there is no per-work-item ACL** (queries/Delivery
Plans do have per-object ACLs). Field-level write exists only via process
rules, which Microsoft says "don't control permissions."

**Delete.** Delete → **Recycle Bin**, "never automatically deletes items,"
full revision preservation; restore needs "Delete and restore work items"
(Contributors have it); **Destroy** needs "Permanently delete work items"
(Project Admins only by default). No archive feature.

**Defaults — open.** "All authorized users can view all defined objects within
the system"; cross-project visibility via Valid Users groups; the "Limit user
visibility" preview has a documented hole: it "applies only to interactions
through the web portal. With the REST APIs … project members can access the
restricted data" — visibility is presentation-layer in places, *weaker* than
the ACL.

**Creator semantics.** None at work-item level; creator-ownership exists only
for queries and Delivery Plans ("Contributors can only edit or delete plans
that they create"). **No crypto enforcement — confirmed** (browser rule-caching
is a UX cache of server rules).

### 6. Linear

**Roles** ([members-roles](https://linear.app/docs/members-roles)): Workspace
Owner (Enterprise only — billing, audit logs, exports), Admin (on Free everyone
is an admin), Team Owner (Business/Enterprise; auto-granted to a team's
creator), Member, Guest (Business/Enterprise — only explicitly-added teams; no
workspace-wide views).

**Read granularity = team.** **Private teams** (Business/Enterprise):
non-members can't see or even @mention into their issues. On Business,
workspace admins can self-join any private team; on **Enterprise, workspace
owners must actively join** — closest to need-to-know. Sub-teams: Restricted
(default) or Private. **No per-issue ACL, no field-level permissions.**
Default: teams public to the whole workspace.

**Delete.** Any user with access can delete an issue (docs specify no role
gate; UNVERIFIED whether Guests can). Deleted issues sit in "Recently deleted"
for **30 days, then permanent removal**; UNVERIFIED-on-current-page: admins
skipping the grace period. Archive is a separate automatic mechanism (no
manual archive).

**Creator semantics: none at issue level** (only team creator → Team Owner).
**Audit log** (Enterprise, owner-only, 90-day UI retention) covers
account/settings events, *not* per-issue changes. **No crypto enforcement —
confirmed**; [linear.app/security](https://linear.app/security) describes
infra-level AES-256 at rest + TLS; the local-first sync engine means access
control is server-side query filtering.

## Part II — Git-native / decentralized trackers

### Fossil

Per-user single-letter **capability codes**, enforced *server-side over
HTTP(S) only*: `n` NewTkt, `r` RdTkt, `w` WrTkt (subsumes r/c/n), `c` ApndTkt,
`q` ModTkt (delete appended comments)
([caps reference](https://fossil-scm.org/home/doc/trunk/www/caps/ref.html)).
Built-in category users `nobody`/`anonymous`/`reader`/`developer` act as role
templates. Docs are explicit that capabilities "only affect accesses over
http[s]://" — SSH/file sync bypasses them, and **a clone gets all ticket
artifacts as bytes** regardless of `r`; ticket read restriction is a web-UI
filter, not a sync boundary. Delete = **shunning**: a hash list that blocks
push/pull re-introduction and physically deletes on `fossil rebuild`; the shun
list deliberately does *not* propagate via sync (prevents remote-triggered
destruction) — the one deliberate tombstone design in this space
([shunning](https://fossil-scm.org/home/doc/trunk/www/shunning.wiki)).

### git-bug

Issues are commit-chains under `refs/bugs/<id>` holding JSON OperationPacks;
ordering via embedded Lamport clocks with validation. Identities are
first-class entities under `refs/identities/<id>` — fast-forward-only version
chains carrying full OpenPGP key sets per version, with time-scoped signature
verification *specified* — **but the spec admits "key management is not yet
fully operational"**: commits are typically unsigned, verification unenforced,
"identity protection" a future feature
([identity spec](https://github.com/git-bug/git-bug/blob/master/doc/spec/identity.md)).
**Access control: none** — trust rests entirely on git-remote push ACLs; anyone
with push can author as any identity. `git-bug bug rm` deletes only the local
ref; a pull resurrects it. A cautionary tale: a fully specified
signing/rotation design that shipped unenforced.

### Radicle (Heartwood) — closest prior art

Primary sources: the [Radicle protocol guide](https://radicle.xyz/guides/protocol)
and [user guide](https://radicle.xyz/guides/user); version-specific details
(e.g. the crefs generalization in 1.3.0+) come from Radicle release notes
and should be re-verified against them before load-bearing use.

- **Identity**: Ed25519 keypair per node; public key = Node ID, encoded as
  **`did:key`**. Permissionless, no recovery/revocation registry.
- **Repository identity**: canonical-JSON **identity document** at
  `refs/rad/id` with `delegates` (DIDs), `threshold` (integer), payloads
  (`xyz.radicle.project`, later `xyz.radicle.crefs`). **RID** = hash of the
  *initial* identity doc — self-certifying (the guide credits TUF as
  inspiration). Identity-doc changes require a **threshold quorum of delegate
  signatures** (`rad id` proposals) — the serverless role grant/revocation
  mechanism.
- **Write authorization is structural + cryptographic**: each peer writes only
  under its own namespace `refs/namespaces/<nid>/…` and signs its entire ref
  listing into **`refs/rad/sigrefs`**; every replicating client verifies.
  Canonical branch = the tip that ≥ threshold delegates identically publish
  (generalized to arbitrary refs via **crefs**, Radicle 1.3.0+).
- **COBs** (`xyz.radicle.issue`, `xyz.radicle.patch`, `xyz.radicle.id`) under
  `refs/cobs/…`: CRDT-style DAGs, one commit per op, materialized by causal
  reduce over all replicated peers; per-op authorization (e.g.
  only-author-edits-description) enforced by every verifying client at
  evaluation time. Append-only; redaction ops exist; physical pruning
  UNVERIFIED.
- **Read control is NOT crypto**: private repos use identity-doc
  `"visibility": {"type": "private", "allow": [dids]}` — selective replication
  by seeds, excluded from inventory gossip, and the protocol guide states
  flatly **"the data is not encrypted at rest."** Whole-repo granularity only;
  any allow-listed seed can leak everything. Transport is encrypted (Noise
  **XK**, NID as static key).

**Verdict**: Radicle proves client-side-verified *write integrity* (signed
namespaces + delegate thresholds) over a git substrate; per-tier *read*
encryption goes strictly beyond anything it ships.

### git-appraise

([repo/docs](https://github.com/google/git-appraise))

Review data as single-line JSON entries in git-notes refs
(`refs/notes/devtools/reviews`, `/discuss`, `/ci`, `/analyses`), merged with
the `cat_sort_uniq` notes strategy (concatenate/sort/dedupe — append-only,
order-insensitive, resurrects "deleted" lines from any peer). **No identity,
signing, or ACL of any kind** — authorship is whatever the pusher writes;
enforcement is the host's push ACL. Read = whoever can fetch.

## Part III — Compliance regimes: required vs nice-to-have

- **SOC 2** (TSC): **CC6.1** logical access restriction (encryption at rest
  and key management are named *points of focus*, auditor-negotiable);
  **CC6.2** register/authorize before credentials + **remove access when no
  longer authorized** — periodic access reviews and deprovisioning records are
  the most-sampled evidence; **CC6.3** role-based least privilege; **CC7.2**
  monitoring supplies the audit-trail expectation. Required in practice: named
  accounts, role-grant records, timely revocation, reviewable access lists.
- **HIPAA** (45 CFR **164.312**, verified against CFR text): §(a)(1) access
  control — *unique user identification* **Required**, emergency access
  **Required**, encryption **Addressable**; §(b) **audit controls a required
  Standard** ("record and examine activity"). "Addressable" means implement,
  substitute, or document why not; encryption is effectively expected (breach
  safe-harbor only for encrypted data).
- **FedRAMP** (NIST 800-53 r5): **AC-2** account management
  (disable-on-separation timelines, periodic review), **AC-3** access
  *enforcement* (technical, not procedural), **AC-6** least privilege
  (+AC-6(9) log privileged functions), **AU-2/AU-12** audit generation,
  **AU-9** protect audit records from modification/deletion — an append-only
  signed log is a direct AU-9 fit.
- **ITAR** — the strongest argument for this design: **22 CFR §120.54** says
  cloud storage/transmission of unclassified technical data is **not an
  export** if secured with end-to-end encryption (FIPS 140-2 modules, ≥AES-128
  strength) and **decryption keys are not provided to any third party
  including the cloud provider**. True client-side crypto with customer-held
  keys makes ciphertext residency and provider access a non-event.
- **GDPR** — **Art. 17** right to erasure is the hard problem for append-only
  git history: tombstoning that leaves personal data recoverable in prior
  commits does not satisfy erasure. Recognized mitigation: **crypto-shredding**
  (destroy the key) — UNVERIFIED: commonly attributed to guidance from the
  Danish DPA, but neither that attribution nor EU-wide acceptance was
  verified against a primary source. **Art. 32(1)(a)** names pseudonymisation and
  encryption as appropriate measures (risk-proportionate).

## Part IV — Crypto reference patterns

- **TUF** ([spec](https://theupdateframework.github.io/specification/latest/)):
  four top-level roles (root, targets, snapshot, timestamp); **delegated
  targets roles scoped by `paths`/`path_hash_prefixes`** with a `terminating`
  flag — direct prior art for path/epic-scoped role delegation; per-role
  signature **thresholds** (unique keyids only); **root rotation** requires the
  new root signed by threshold of *both* old and new root keys (old quorum
  blesses new); rollback/freeze protection via version monotonicity + expiry +
  snapshot binding; all keys offline except timestamp.
- **SOPS/age**
  ([SOPS README](https://github.com/getsops/sops), v3.9.0): per-file data
  key wrapped to each recipient
  (age/KMS/PGP) in the file's `sops:` metadata; `.sops.yaml` `creation_rules`
  by `path_regex`; `key_groups` + `shamir_threshold` gives real threshold
  decryption. Rotation semantics matter: **`sops updatekeys` rewraps without
  changing the data key; `sops rotate` generates a new data key** — the README
  warns removed recipients "may have had access to the data key in the past."
  Keys/structure stay cleartext; MAC covers values but binds the file to
  itself only — **no rollback protection across commits**.
- **Matrix Megolm / Signal groups**
  ([Megolm spec](https://gitlab.matrix.org/matrix-org/olm/-/blob/master/docs/megolm.md),
  [Matrix E2EE spec](https://spec.matrix.org/latest/client-server-api/#end-to-end-encryption),
  [Signal Private Group System paper](https://eprint.iacr.org/2019/1416)):
  Megolm's forward-only ratchet means
  sharing state at index i grants read from i onward, never before — stated
  concretely because the forward/backward-secrecy vocabulary is used
  inconsistently across the literature: compromise of a session state
  exposes all *subsequent* messages in that session and none *before* it
  (the hash ratchet cannot be reversed), so clients
  **rotate the outbound session when a member leaves** plus
  `rotation_period_ms` (default 604800000) / `rotation_period_msgs` (default
  100). Signal Private Group System: group state encrypted under a
  **GroupMasterKey** the server never has; membership proven via
  keyed-verification anonymous credentials. Sender Keys (WhatsApp whitepaper):
  member removal forces every remaining sender to issue a fresh sender key.
  This is THE reference for tier revocation: removed members keep old epochs;
  new epoch keys go to survivors only.
- **Monero view keys**
  ([moneropedia: view key](https://www.getmonero.org/resources/moneropedia/viewkey.html)):
  private view key = see incoming; spend key = write.
  View-only wallets "cannot reliably view outgoing transactions" — lesson: a
  read capability carved from a write keypair can be *lossy*; audit which
  observations require the write key. (Key-image explanation
  UNVERIFIED-as-quoted.)
- **CT + witness cosigning**
  ([RFC 6962](https://www.rfc-editor.org/rfc/rfc6962),
  [C2SP tlog-witness](https://github.com/C2SP/C2SP/blob/main/tlog-witness.md)):
  RFC 6962 STHs + Merkle **consistency proofs**;
  §7.3: "two conflicting Signed Tree Heads for the same log … is cryptographic
  proof of that log's misbehavior"; the C2SP **tlog-witness** protocol (used by
  Sigsum) has witnesses atomically verify a consistency proof from their last
  cosigned checkpoint (409 on size mismatch, 422 on bad proof) — a quorum of
  independent cosignatures makes split-view infeasible. This is the
  client-side defense against a dumb git remote serving different histories to
  different members.
- **Tahoe-LAFS**
  ([architecture docs](https://tahoe-lafs.readthedocs.io/en/latest/architecture.html)):
  capability lattice **write-cap ⊃ read-cap ⊃ verify-cap**
  (read-cap = hash of public key; write-cap contains hash of private key);
  attenuation is structural ("transitively read-only" directories); repairers
  hold verify-caps without decryption keys. Honest caveat: capabilities are
  "expressly delegated (irrevocably) by simply transferring the relevant
  secrets" — Tahoe solves separation, not revocation.
- **git-crypt / git-remote-gcrypt / Keybase git**
  ([git-crypt](https://github.com/AGWA/git-crypt),
  [git-remote-gcrypt](https://spwhitton.name/tech/code/git-remote-gcrypt/),
  [Keybase encrypted git](https://keybase.io/blog/encrypted-git-for-everyone)):
  git-crypt = per-file
  smudge/clean, deterministic AES-256-CTR (doesn't hide change/equality
  patterns), symmetric key GPG-wrapped per collaborator; **"git-crypt does not
  support revoking access … a user having the previous key can still access
  previous repository history."** gcrypt = whole-history encrypted packfiles +
  GPG-signed manifest; `gcrypt.participants` doubles as the write-auth check;
  "every git push effectively has --force"; some backends re-upload full
  history per push. Keybase git encrypts repo *and branch names* and —
  uniquely — **enforces signature verification on fetch** (integrity binding
  between content and history); server still sees team membership and
  push/fetch metadata; granularity per-repo-per-team (team-key rotation on
  removal UNVERIFIED in the git-specific post). **Nothing existing does
  per-object tiered visibility inside one git history — that is the genuinely
  novel part of the lit design.**

## Part V — Cross-cutting synthesis

**(a) Recurring role/capability vocabulary.** The five role-based products
converge on a ~5-step ladder: *no-access → viewer/reader →
limited-contributor (Guest/Triage/Planner) → editor/member → admin/owner*;
ADO is the structural outlier, an identity/Allow-Deny bitmask model rather
than a named ladder. Three patterns recur where the evidence documents
them: (1) **verb-split permissions** rather than
monolithic "write" (assign, transition, resolve, modify-reporter, set-security
are separate grants in Jira; close/reopen/delete separate in GitHub custom
roles); (2) **Own-vs-All splits** for comments/attachments (Jira's "Delete own
comments" vs "Delete all comments"); (3) **dynamic principals** — Jira's
Reporter/Current-assignee grant targets and GitLab's author-or-assignee
disjunction let ticket-relative identity substitute for role. Verb-splits
recur across all six products; the own/all split is documented here for
GitHub, Jira, and GitLab (Rally's and Linear's comment permissioning was
not surveyed); dynamic principals appear in Jira and GitLab. That
recurrence — strongest for verb-splits, solid for own/all and relative
principals among the permission-rich products — is the yardstick any
capability vocabulary in this space gets measured against.

**(b) The granularity floor.** Read: container-level
(repo/project/team/area-path) is the universal baseline; the products
customers pay most for add **per-issue security (Jira issue security schemes,
GitLab confidential issues) and per-comment restriction (Jira restricted
comments, GitLab internal notes)** — that's the evident ceiling of demand;
nobody offers per-field read restriction. Write: verb-level per container,
with per-issue write reduction only via author/assignee dynamics. A
per-visibility-tier encryption design that supports (project-tier, issue-tier,
comment-tier) matches the best-in-market surface; key-per-tier-per-container
is tractable where key-per-ticket is not — and note the granularity of the
*grant targets*: GitLab's confidential issues gate on fixed role thresholds
only, and Jira's security levels are *defined* in terms of roles, groups,
and dynamic principals — though a Jira level's membership can also name
individual users and per-issue picker fields, which is genuine per-item
ACL machinery at the high end. The role/level-shaped core maps cleanly onto
tier keys; the user-picker tail is the part tier keys deliberately don't
chase.

**(c) Delete is never just delete.** Three products have a true
recoverable-then-destroy split for work items: ADO Recycle Bin (never
auto-purges; "Permanently delete work items" is a distinct permission that
even collection admins can be Denied), Rally Recycle Bin (owner or Editor
deletes; only subscription/workspace admins purge), and Linear (30-day
trash). Jira sits adjacent: Archive+Restore are permissions distinct from
Delete, but Delete itself is not documented here as recoverable — archive
is a separate lifecycle, not a delete trash-state. GitHub gates issue
delete hard (org-opt-in + admin + audit event) with no recoverable step;
its tombstone machinery ("minimize") exists only at comment level. GitLab
is the outlier (broad hard delete, no issue audit events) and reads as the
anti-pattern. The pattern that survives the details: destruction is
consistently *harder-gated* than deletion, and where a recoverable
intermediate exists it is the ordinary-privilege path. Fossil's non-propagating shun list is the one
design built for a sync'd substrate: a tombstone that blocks re-introduction
without letting a remote trigger destruction. The recurring pattern for a
synced or encrypted substrate: soft delete as an ordinary recorded mutation,
irreversible destruction as a separate harder-gated act — and in encrypted
systems, key destruction is the only destruction ciphertext admits (the GDPR
Art. 17 / ITAR §120.54 connection).

**(d) Creator-owns-ticket precedent.** Real but partial: GitHub (author closes
own issue, roleless), GitLab (author-or-assignee close/reopen/edit; author-only self-delete),
Jira (Reporter as a grant target in schemes *and* as an issue-security-level
member), Rally (owner-may-delete-own-item). ADO and Linear have none at item
level. So creator-ownership as a default capability is well-precedented for
close/edit and even delete-own — lit making it cryptographic (the creator's
signature is the capability) would formalize what incumbents implement as
special-cased ACL logic.

**(e) No incumbent has any cryptographic or client-side enforcement —
confirmed for all six products.** The closest gestures: Atlassian BYOK/CMK
(at-rest key custody; server still decrypts to enforce), Linear infra AES-256,
ADO's visibility limits that its own docs admit are web-portal-only (REST
bypasses them). Among git-native systems, Radicle is the sole prior art for
client-side *write* enforcement (Ed25519 sigrefs + delegate-threshold identity
docs, TUF-inspired self-certification) but explicitly does not encrypt at rest
— read privacy is trusted-seed allow-lists. Nobody anywhere combines
signed-mutation write control with per-tier read encryption in one git
history.

**(f) What compliance actually requires vs nice-to-have.** Required: unique
per-user identity (HIPAA 164.312(a) Required spec), enforced-not-procedural
authorization (AC-3), least privilege with revocation/deprovisioning timelines
(CC6.2/6.3, AC-2/AC-6), and a tamper-resistant activity log (164.312(b)
required Standard; AU-2/AU-12; AU-9 protection — where a signed append-only
git log is a *better-than-incumbent* fit, given GitHub logs only 180 days and
GitLab logs no issue events at all). Nice-to-have-but-decisive: encryption
(HIPAA Addressable, GDPR Art. 32 "appropriate", SOC 2 point-of-focus) — except
under **ITAR §120.54**, where customer-held-key E2E encryption is the
difference between cloud storage being an export event or not. The regimes'
one structural demand that fights crypto: revocation and erasure. Every
studied crypto system (Megolm, Sender Keys, SOPS, git-crypt) delivers only
**forward-only revocation** — epoch/key rotation for survivors, never
retroactive. Rotation alone therefore cannot satisfy an erasure demand;
crypto-shredding (Part III) is the recognized mitigation for that gap — a
compliance-side answer, not a documented feature of the studied systems.
CT-style consistency proofs + witness/cosigning among clients are the
studied defense for rollback and equivocation detection against an
untrusted store. Note also that ACL-style
**Deny is unimplementable cryptographically** (you cannot un-give a key):
every studied E2E group system is additive-grant with rotation for exactly
this reason.
