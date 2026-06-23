# lit for agents — install, set up, and work

Audience: an AI coding agent being onboarded to use `lit` (`links-issue-tracker`) in a
project. This is the setup-and-orientation doc. For the live, authoritative command
reference, run `lit quickstart` once `lit` is installed — this guide complements it, it
does not replace it.

You should be able to follow every step here without asking a human. Every command below
is real and behaves as written.

## 1. Check whether lit is already available

```sh
lit version
```

- Prints a version and `schema versions supported: ...` → `lit` is installed. Skip to step 3.
- `command not found` → install it (step 2).

## 2. Install lit

`lit` ships **prebuilt, self-contained binaries** — no Go toolchain, no system ICU/zstd, and
no `dolt` CLI needed at runtime (the storage engine is compiled into the binary). Install the
latest release onto your `PATH`:

```sh
curl -fsSL https://raw.githubusercontent.com/promptctl/links-issue-tracker/master/scripts/install.sh | bash -s -- --latest-release
```

This downloads the binary for your OS/architecture, checksum-verifies it, installs it to a
directory already on your `PATH` (or `~/.local/bin`), and warns if any *other* `lit` on
`PATH` would shadow it. If it warns, remove the stale binaries — otherwise the `lit` you run
will not be the one you installed. Needs `curl`, `tar`, `jq`; on Windows run it from Git
Bash. Verify:

```sh
lit version
```

If there's no prebuilt archive for your platform, or you're developing `lit` itself, build
from source instead (needs the **Go toolchain** and a git checkout):

```sh
git clone https://github.com/promptctl/links-issue-tracker
cd links-issue-tracker
./scripts/install.sh        # no flags = build from source
```

Full matrix, manual download, version pinning, and the macOS ICU/zstd build notes are in
[introduction/installation.md](introduction/installation.md).

## 3. Initialize the workspace (once per clone)

Run this from inside the target repository:

```sh
lit init
```

This creates the issue store under `$(git rev-parse --git-common-dir)/links/` and adds a
short `lit` section to `AGENTS.md` and `CLAUDE.md` so future agents know to run
`lit quickstart`. If the repo's remote already carries `lit` ticket data, `init` adopts
that backlog automatically, so a fresh clone starts with the project's real tickets
rather than an empty store. Useful flags:

- `--skip-hooks` — don't install the git sync hook
- `--skip-agents` — don't touch `AGENTS.md` / `CLAUDE.md`

Already initialized? `lit init` is safe to run again; it reconciles the integration blocks.

To install the per-clone sync automation separately:

```sh
lit hooks install
```

## 4. The core work loop

In a repo that's already set up, **your first action is always:**

```sh
lit quickstart
```

It prints the current, authoritative workflow. The loop it describes is:

```sh
lit ready                 # what's workable now, top item first (or: lit next / lit backlog / lit queue)
lit show <id>             # read the full ticket before you touch code
lit start <id>            # claim it (moves it to in_progress, assigns it to you)
# ...do the work...
lit comment add <id> --body "Starting: <plan>"   # leave a trail as you go
lit done <id>             # finish — closes the ticket and prints follow-up capture guidance
```

`lit done <id>` closes the ticket and prints post-close guidance: a prompt to capture, while
context is fresh, anything the next agent needs (follow-ups, comments on adjacent tickets).
Verifying the work is correct belongs *before* the merge — in CI and PR review — not in this
post-merge close, which runs after the change has already landed.

Other commands you'll need:

```sh
lit new --title "..." --topic <slug> --type task|bug|feature|chore|epic [--parent <id>]
lit close <id>            # close without finishing (wontfix / obsolete / duplicate)
lit followup --on <closed-id> --title "..."   # capture surfaced work while context is fresh
lit doctor [--fix]        # health check; run --fix yourself before escalating any error
```

## 5. Two things that will bite you

- **The store you see depends on your current directory.** `lit` selects the database from
  `git rev-parse --git-common-dir` of your cwd. If you `cd` into a different repo (or a
  nested checkout), you are silently looking at a *different* backlog. If `lit ls` shows
  tickets you don't recognize, check where you are: `lit workspace`.
- **A stale binary lies.** If a documented `lit` subcommand prints an unrelated usage error,
  the installed binary is older than the source. Rebuild it onto your `PATH`:
  `go build -o "$(which lit)" ./cmd/lit` from the checkout.

## 6. Finishing

Always commit your code changes. A ticket is done when the work is validated, reviewed, and
merged — not merely when the code compiles. When in doubt, leave the ticket `in_progress`
and report status rather than closing it prematurely.

---

See also: the human-facing [README](https://github.com/promptctl/links-issue-tracker#readme) and `lit quickstart` (the live command
reference, which always reflects the installed binary).
