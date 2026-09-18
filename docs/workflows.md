# Workflows

Workflows are guidance you write yourself, which `lit` injects into its own output at
declared moments in the work lifecycle — a ticket entering `in_progress`, a `lit done`
closing one, an agent viewing the backlog. One definition is a markdown file with YAML
frontmatter: the frontmatter says when to fire, the body says what to inject. Nothing
here is enforced. A definition only adds text to what a command already prints; it
never blocks or gates anything.

A definition fires on a label the ticket carries, a state it enters or leaves, a
semantic event a command dispatched, or any combination of the three. Because
definitions bind to events rather than to command names, a command can be renamed
without breaking guidance somebody wrote a year ago.

Definitions are found in three layers: `.lit/workflows/` in the repository, checked in
and shared with the team; a `workflows/` directory under your own `lit` config; and the
defaults shipped inside the binary. The nearest layer wins outright — a project file
replaces a global or embedded one carrying the same id whole, never field by field —
so customizing the guidance `lit` ships with means copying that definition down into
the repository and editing it there.

Run `lit workflows --help` for the rest: the frontmatter keys and what a minimal
definition looks like, how the three activation dimensions combine (alternatives within
one dimension, all-of across them), the full catalog of events and which command
dispatches each one today, and what the `show`, `edit` and `dry-run` subcommands do.
`lit workflows edit <id-or-point>` is how you start a new definition or override a
shipped one; it scaffolds the file and prints its path.

Run `lit workflows` with no arguments to see the lifecycle spine of the repository
you're in: every event, state and label anything is bound to, the definitions active at
each point, the layer each one resolved from, and a warnings section listing the files
that loaded but can never fire.

If you used the `guidance-<action>-<phase>.md` templates from earlier `lit` versions,
workflows replace them — the same idea of injecting text at a moment in the lifecycle,
now declarative, user-authored, and not tied to a fixed set of command names.
