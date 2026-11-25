# Workflows

Workflows are guidance you write yourself, which `lit` injects into its own output at
declared moments in the work lifecycle — a ticket entering `in_progress`, its work
moving into `review`, a ticket being closed, an agent viewing the backlog. One definition is a markdown file with YAML
frontmatter: the frontmatter says when to fire, the body says what to inject. Nothing
here is enforced. A definition only adds text to what a command already prints; it
never blocks or gates anything.

A definition's `when:` lists the moments it fires at: the ticket's state changing into
or out of a state (`enter closed`, `exit plan`), or `lit` showing or recording
something (`show_ticket`). `labels:` narrows it to tickets carrying a label. A change
of state fires as the crossing it makes, whichever command makes it: `lit done` and
`lit close` both enter `closed`. Because definitions bind to moments rather than to
command names, a command can be renamed without breaking guidance somebody wrote a
year ago.

Definitions are found in three layers: `.lit/workflows/` in the repository, checked in
and shared with the team; a `workflows/` directory under your own `lit` config; and the
defaults shipped inside the binary. The nearest layer wins outright — a project file
replaces a global or embedded one carrying the same id whole, never field by field —
so customizing the guidance `lit` ships with means copying that definition down into
the repository and editing it there. `lit` ships `done`, which fires on entering `closed`, and
one definition for entering each phase — `phase-plan`, `phase-impl`, `phase-review`,
`phase-merge` — that names the command recording the work's next move.

Run `lit workflows --help` for the rest: the frontmatter keys and what a minimal
definition looks like, how `when:` and `labels:` combine (alternatives within one key,
all-of across them), the built-in states and the events with the command that fires
each one today, and what the `show`, `edit` and `dry-run` subcommands do.
`lit workflows edit <id-or-point>` is how you start a new definition or override a
shipped one; it scaffolds the file and prints its path.

Run `lit workflows` with no arguments to see the lifecycle spine of the repository
you're in: every moment a `when:` entry can name and every label anything is bound to,
the definitions active at each, the layer each one resolved from, and a warnings
section listing the files that loaded but can never fire. A command that fires a moment
prints the same warnings on stderr.

If you used the `guidance-<action>-<phase>.md` templates from earlier `lit` versions,
workflows replace them — the same idea of injecting text at a moment in the lifecycle,
now declarative, user-authored, and not tied to a fixed set of command names.
