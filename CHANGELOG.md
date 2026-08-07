# Changelog

All notable changes to `links-issue-tracker` (the `lit` CLI) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `lit show <id> --field <name>[,<name>...]` prints exactly the requested field(s) and nothing else — no header fields, no parent-epic body, no siblings, no epic/children summary. A single field prints its bare value (e.g. `lit show <id> --field description`), so it round-trips directly into `lit update --description`; multiple fields print as `name: value` lines. An unknown field name fails with a `UsageError` listing the accepted vocabulary, with no partial output. Omitting `--field` leaves the existing full-detail view unchanged.
- `lit import --path <file.yaml>` bulk-creates and bulk-updates issues from one multi-document YAML file, one issue per document, selected per document by an `id` field: absent, the document creates (same required fields as `lit new`, plus `local_id`/`parent`/`depends_on` wiring — `parent`/`depends_on` may name either another document's `local_id` or an existing real issue ID); present, the document patches that issue with the same field set `lit update` exposes (`title`, `description`, `prompt`, `type`, `priority`, `assignee`, `labels`, `lane`, `reason`). A mixed file (some create, some update) is legal; an `id` matching no existing issue is a hard error, never a silent create. This is additive to `lit import`'s existing JSON tree-spec format (still supported, dispatched by any extension other than `.yaml`/`.yml`) — one command, format chosen by the file's extension.
- Groundwork for `lit workflows` (user-customizable guidance injection at work-lifecycle moments): the workflow-definition model. Markdown files with YAML frontmatter are discovered recursively under `.lit/workflows/` (project) and `<config>/workflows/` (global) — the folder hierarchy is arbitrary; frontmatter alone declares where a definition activates, via `labels:` / `states:` (with `when: enter|exit`) / `events:` dimensions (OR within a dimension, AND across dimensions). Nearer layers override farther ones by `id` (defaulting to the file's relative path with the `.md` suffix dropped and spaces replaced by underscores). The semantic event catalog (`show_backlog`, `show_ticket`, `work_started`, …) is the stable contract definitions bind to — never command names. Not yet wired into any command; the event dispatch, injection, and `lit workflows` CLI land with the rest of the epic.
- More `lit workflows` groundwork: every curated agent-facing command (`backlog`, `show`, `next`, `new`, `followup`, `update`, `comment add`, and `start`/`done`/`close`/`open`) now dispatches its semantic event, in-process and synchronously, at the point its read or write already succeeded. Still no observable effect — matching definitions against the dispatched event and injecting their guidance is the next ticket in the epic.

### Changed

- Emitted `<agent-instructions>` guidance and remediation text no longer commands the agent to bypass or withhold from the user, and no longer uses ALL-CAPS `MUST NOT` / `do NOT` imperatives — the same intent is now carried as declarative facts and consequences (e.g. "this command is idempotent and safe to run without confirmation" instead of "do NOT ask the user"). Affected surfaces: sync-failure blocks (`lit sync`, `lit doctor`), `lit doctor --fix`/GC-contention/corruption remediation, the rank-inversion warning, the empty-store guard in `lit init`'s remote adopt, the first-push skip message, the prose-conflict reconcile guidance, and the `lit quickstart` `<agent-instructions>` preamble.

### Fixed

- `lit show` on a ticket with an epic parent no longer prints its siblings twice. The `siblings:` group duplicated the same ids the `Epic: ... Children:` block already lists (with richer per-child status and a `(you are here)` marker for the shown ticket) — the redundant group is removed; the epic block remains the single place sibling membership and position are conveyed.

### Removed

### Security

## [0.3.0] - 2026-08-02

### Added

- The release is now hard-gated on the license posture: a dedicated `license-gate` CI job runs the policy check over every component compiled into the release binary, and the publish job depends on it. If any dependency (Go module or native library) carries a non-allowlisted license, the release aborts before any tag, archive, or asset is published — a non-free build cannot be released, even from a branch that skipped the PR-time check.
- The SBOM, `THIRD_PARTY_LICENSES` bundle, license report, and license-policy gate now also cover the statically-linked native C libraries that cgo compiles into release binaries but no go.mod tool can see: ICU 75.1 (Unicode-3.0), zstd 1.5.6 (BSD-3-Clause), musl 1.2.5 (MIT), and compiler-rt via zig 0.14.0 (MIT). Each appears with its name, version, license, `pkg:generic` PURL (SBOM), and verbatim notice text (bundle). Versions are pinned to the release build config, with a CI check tying ICU/zig to `build/Dockerfile.release`.
- CI license-policy gate: every linked module's license is checked against a committed allowlist (`tools/licenses/policy.json`) of permissive licenses plus documented per-module exceptions, and the build fails if a dependency bump pulls in a non-allowlisted license (GPL/AGPL/SSPL/BUSL, etc.). Runs on every pull request and master push via `go test` (and standalone via `go run ./tools/licenses -check`). Shares the classifier with the license report/SBOM, so the gate checks the exact licenses those artifacts document.
- Every release now ships a CycloneDX SBOM (`lit_<version>_sbom.cdx.json`) as a standalone downloadable asset: a machine-readable bill of materials listing every Go module and statically-linked native C library compiled into the binary, with its version, package URL, and resolved license. It is generated from the same linked-module inventory as `THIRD_PARTY_LICENSES`/`LICENSE-REPORT.md`, validated as CycloneDX 1.6 in CI, and lets vulnerability scanners audit a given `lit` version against CVE feeds.
- CI gate (`TestReleasedMigrationsAreContentPinned`) that refuses any change reusing a released migration version number under different content — the mechanism that bricked workspaces in the migrate-drift epic. Every non-baseline migration's content is pinned by sha256; reusing, editing, deleting, or leaving a version number unpinned fails the build with a message naming the collision.

### Changed

- `lit --help` now presents the state-transition verbs in two groups so the high-traffic status lifecycle stands out: the core verbs (`start`, `done`, `close`, `open`) stay under **Agent Operations**, while the low-traffic retention quartet (`archive`, `unarchive`, `delete`, `restore`) moves to its own **Issue Retention** group. The grouping mirrors the model's `StatusAction`/`RetentionAction` partition; every verb stays a first-class command — only its placement in help changed, never its dispatch.
- `lit ls` gained an `--at <store-dir>` flag that lists a discovered store (a path from `lit stores`) read-only, without depending on the current directory being a lit workspace. This is the folded-in former `lit ls-at`, now a flag on `ls` rather than a separate command — and it is strictly more capable, since every `ls` filter/sort/column/format now applies to a foreign store too (the old command was hard-wired to open + in_progress).
- `lit stores` gained a `--counts` flag that reports each discovered store's ready / in-flight / blocked counts as a cross-project rollup instead of listing storage paths. This is the folded-in former `lit overview` — the same discovery walk and store-intrinsic readiness classification, now a flag on `stores`.
- `lit parent set` now renders the parent-child edge through the same canonical projection `lit dep` uses, so the identical edge reads the same way whichever command wrote it (`<child> --child-of--> <parent>`, previously `--parent-child-->` from `parent set` alone). Both commands already wrote through one store owner; this removes the last cosmetic divergence between the two faces of the one edge.
- Command summaries in `lit --help` now name the mechanism behind the three snapshot-shaped commands so they can be told apart: `export`/`backup` are the JSON **data-export** family (portable tree out; `backup` wraps it with rotation), while `snapshots` is the Dolt **filesystem-level database** snapshot mechanism. The two mechanisms are unchanged — only the naming ambiguity is fixed.
- `lit quickstart` and the agent-facing prompt text now describe only the curated command surface: the finding-work fastpath is `lit next` → `lit start <id>` (was `lit ready`), the workable views are `lit next` / `lit backlog` (the retired `ready` / `queue` are gone), and no quickstart page, README, or getting-started walkthrough example invokes a command that now hard-errors. This brings the guidance (the map) into line with the surface the earlier children of this epic shipped (the territory).
- The `lit quickstart ready` guidance topic is renamed to `lit quickstart work` — its name no longer collides with the retired `ready` command, and it reads as what it is (finding *and* starting work, over `next`/`backlog`/`start`). The `lit start` success breadcrumb now points at `lit quickstart work`, and the `--eject` short-name is `quickstart-work`. An old `lit quickstart ready` invocation errors with the valid topic list (`work, new, update, done, doctor`); downstream `CLAUDE.md`/`AGENTS.md` integration blocks are unaffected, since they only reference bare `lit quickstart`. **If you ejected and customized the old topic template**, rename your override from `quickstart-ready.md` to `quickstart-work.md` (in `.lit/templates/` or the global templates dir) — the guidance now resolves under the new name, so an override left at the old filename is silently ignored in favor of the embedded default.

### Fixed

- Sync reconciliation of branches with unrelated histories no longer depends on a silent error-swallowing bug in the embedded Dolt driver. The driver's first-row pre-read discarded any error, so a query that failed on its first row (such as `DOLT_MERGE_BASE` raising "no common ancestor" for refs that share no history) could surface as an empty result set instead of the real error. The driver now propagates that error, and merge-base resolution recognizes the "no common ancestor" backend error as the unrelated-histories state directly — the same domain outcome it already handled via an empty result set.

### Removed

- Three single-purpose commands are folded into flags on broader commands and retired as standalone verbs, each with a documented pointer (no capability lost): `lit assign <id> <name>` → `lit update <id> --assignee <name>` (reassigning is a single-field write; `update` already owns field mutation, including the optional `--reason`); `lit ls-at <dir>` → `lit ls --at <dir>`; `lit overview` → `lit stores --counts`. The retired verbs stay dispatchable but hidden, so an old invocation returns a pointer to the surviving flag (exit 3) instead of cobra's bare unknown-command error — the break is deliberate and named, never silent.
- `lit bulk import` is retired: it was a second name for the export-restore that `lit backup restore --path <export.json>` already owns (both ran the identical restore path), and it was the odd verb out in a family of per-`--ids` fan-out operations. An old `lit bulk import` invocation now returns a documented pointer to `lit backup restore`. `lit import` remains the one home for JSON tree-spec imports — a distinct operation that only shared the word "import."
- `lit update --status` is retired: `update` no longer changes an issue's status. The transition verbs (`lit start` / `lit done` / `lit close` / `lit open`) are now the single enforcer of the transition guardrails, so `update --status` is rejected with a documented pointer to them instead of routing status through a second, divergent path. This closes the former `--status closed` back door, which recorded a resolution-less `done` and bypassed `close`'s required outcome. The capability is fully reachable under the verbs; the break is deliberate and named, never silent. `update` remains the home for field edits (`--title`, `--description`, `--assignee`, `--labels`, `--lane`, etc.).
- The `ready` and `queue` workable views are retired from the command surface, leaving `next` (the single leaf to start) and `backlog` (the full ranked queue with blocked items shown inline) as the only named workable views — `ls` remains the power query and `orphaned` its own staleness view. `lit ready` and `lit queue` no longer appear in `lit --help` or shell completion, and now fail with a documented pointer to `lit backlog` / `lit next` instead of running: the break is deliberate and explained, never silent. The retired presentations (ready's blocked-to-bottom re-sort and coaching preamble; queue's terse pullable-only list) are dropped — `backlog` and `next` carry the surviving intent.

### Security

## [0.2.1] - 2026-08-01

### Changed

- Rewrote lit's agent-facing prompt surfaces (the CLAUDE.md integration block `lit init` writes, and the `lit quickstart` guidance) into calm, plain, second-person tool documentation. The previous text used the fingerprint of a prompt-injection attack — ALL-CAPS imperatives, threats, and act-behind-the-user framing — which led a security-conscious agent to flag lit as untrustworthy and advise uninstalling it. The agent-native workflow is unchanged; only the register is.

## [0.2.0] - 2026-07-30

### Added

- In-repo issue tracking backed by an embedded [Dolt](https://www.dolthub.com/) database — the backlog ships with the code and every mutation is a versioned commit.
- Agent-first core loop: `lit ready`, `lit start`, `lit done`, and `lit followup` return guidance alongside data so a coding agent can drive the workflow unattended.
- `lit quickstart` prints the live command reference for agents.
- One write boundary and one identity per clone, keyed off `git rev-parse --git-common-dir`, so all worktrees of a clone share a single issue view.
- `lit version` command with build-time `-ldflags` version stamping.
- `lit doctor [--fix]` for diagnosing and repairing workspace issues.
- `lit lifeboat` recovery path for schema-ahead workspaces.
- `scripts/install.sh` builds `lit` from source and installs it onto `PATH`, warning about stale shadowing binaries.

### Changed

- `lit new` and `lit followup` now rank new tickets to the **top** of the order by default (fresh work surfaces first) instead of the bottom. Pass `--bottom` to append at the bottom — use it when authoring a batch in order so creation order is preserved. Importing an existing backlog still preserves its order.

### Fixed

### Removed

- The `--json` flag is removed from every command. The default human-readable text is the one canonical surface — it is already the agent interface, and per-line output is parseable when a script needs one field (e.g. `lit workspace | sed -n 's/^traces_dir: //p'`). `lit export` and `lit lifeboat dump` are unchanged: they emit a JSON data structure as their sole output (no flag), because a full export / raw dump has no text form. Passing `--json` now fails with a usage error.

### Security
