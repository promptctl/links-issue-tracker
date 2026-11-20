# A path-addressed CLI

**Status:** accepted design, 2026-11-02. Written from one grooming session on a
59-row backlog and the conversation that followed it. Nothing here is implemented.

## The problem, measured on one session

A grooming pass over `go-template-js` read every open ticket, closed five, merged
four, moved one, and rewrote thirty descriptions. The work itself was ordinary. The
cost was in the tool:

- Reading 59 tickets cost ~270 KB of output, of which ~40 KB was new information.
  `lit show` on a child reprints its epic's whole body and the full sibling plan every
  time. Four tool calls went to writing `awk` filters to strip the repetition.
- Leaving one comment took three round trips: `lit comment` → usage error →
  `lit comment add` → usage error → `lit comment add --body`. Each verb has its own
  grammar: `close` takes `--reason`, `update` takes `--description-file`, `done` takes
  nothing.
- Every mutating command printed `deeper guidance: lit quickstart update` and nothing
  else. Whether the update applied had to be re-queried.
- `--lane` existed the whole time. It appears in `lit update --help` and nowhere else:
  not in `lit --help`, not in `lit quickstart update`. The Unfiled epic's description
  said "every ticket here is its own lane" as if the description were the mechanism.
  Eight `lit update <id> --lane <id>` calls were the fix, found only when told the flag
  existed.
- `lit done` on a ticket left a claim on its sibling for this checkout. Closing
  something started something, and nothing said so.

None of these are bugs. They are the shape of a CLI designed for a human reading one
ticket with the docs open, used by an agent that reads thirty, writes thirty, and
holds none of the prose.

## Three rules

1. **It works for everything or it works for nothing.** Any carve-out ("start and done
   stay special verbs") makes the addressing scheme a veneer and puts the table back
   in the position of lying about what exists. `[LAW:no-mode-explosion]`: a verb that
   escapes the grammar is a mode, and the grammar's value is that it has none.
2. **Derived state is never an action the agent must remember, and never a surprise.**
   Claim-by-construction is right: there is no "remember to claim your lane". What was
   wrong in the session was not the claim, it was that nothing echoed it.
   `[LAW:nothing-unseen]`: every derivation a write triggers is printed with the write.
3. **What worked in the current design was commitment, not rank.** Once an agent is in
   an epic, `next` keeps serving it until the unit is finished. That is the behaviour
   the old mess lacked. It works today because the epic happens to be the claim unit
   and sibling order happens to be the sequence; rank among individual tickets was
   never doing the useful work, and reranking individual tickets is what scattered the
   backlog before. The behaviour is almost right for the wrong reasons.

## The model

A workspace is a tree. Every node has a path. Four verbs operate on paths. There is
nothing else.

### Paths

A path is a node, then `/`, then a field, then a sub-id, as deep as the tree goes.
Ticket ids keep `.` for children, as today; `/` descends into a node's fields. A path
beginning with `/` is anchored at the workspace root.

```
dee                     the ticket
dee/title               scalar
dee/body                scalar
dee/status              scalar, validated against the column graph (below)
dee/labels              list
dee/comments            list; elements have system-minted ids
dee/comments/43s        one comment
dee/comments/(by=claude_64e3)   one session's comments: a selector filters any step
dee/blocks              list of ticket ids
dee/parent              scalar ticket id
dee/claim               scalar, derived, not settable
dee/stage               scalar, derived: initial, active, or terminal (below)
792a.dee/body           a child's field: `.` for the child, `/` for the field
/columns                the root's column graph
/lanes/datetime         a lane at the root
/lanes/dee              the one-element lane a ticket in no lane is
/integrity              a computed field on the root
```

The two separators do different jobs and cannot collide: `.` is part of an id and
only ever precedes a minted child id, `/` only ever precedes a field name. No reserved
words, so an id can be anything the minter produces. An id is its segments, and a
ticket is addressed by its own segment, as the ids above are written; the parent
segments, the prefix and the ticket's `topic` are rendering,
`<prefix>-<topic>-<segments>`, and the topic is what tells one run of segments from
another at a glance. A segment two tickets share is refused naming both.

Selectors, globs, lists, and objects are made of shell metacharacters, so on a command
line they are single-quoted: `get '(status=backlog)/title'`, `set 'rin.*/priority' urgent`.
Unquoted, zsh reports no match and bash splits them, before lit runs.

`show` prints exactly these paths, with `$` standing for the ticket, so what is read
is what is typed:

```
$ = template-unfiled-792a.dee
$/title         dateInZone accepts zone names in any letter case
$/lane          datetime
$/comments/43s  [claude_64e3…] Facts from PR #43 …
```

`[LAW:parse-dont-validate]`: a path is parsed once into a typed address (node kind,
field kind, scalar-or-list, settable-or-derived) and every verb takes the address, not
the string. An address to a derived field is not a valid target for `set`, so
`set dee/claim x` fails at parse, not inside `set`.

### Verbs

```
VERB  TARGET          DOES
get   path...         read. Many paths; shared ancestors printed once.
set   path value...   replace a scalar or a whole list. Pairs. Echoes old → new per path.
add   path value      append to a list. The system mints the id and echoes it.
rm    path...         delete a node: a ticket, a comment, a label, a dep.
```

Global on every write: `--dry-run` prints the same diff the write would echo, and
performs nothing. `[LAW:effects-at-boundaries]`: a write computes a description of
its change and hands it to the edge; dry-run is the edge declining to perform it.
Also on every write: `--reason "…"`, carried on every event the write mints and
printed in `get x/history` beside the change, so the why travels with the what.
On `get`: `--sort <path>` orders the matches by a scalar field, `--desc` reverses,
`--limit N` keeps the first N; without them matches print in backlog order.

`set` on a list replaces the list. `add` appends; a list value appends each element.
The two are the only list operations, and `set` echoes every element it removed, so a
replacing typo is visible in the echo and reversible from it; `--dry-run` shows it
before it happens.

A write with many paths is one write: a selector or glob expands once, before
anything is written, and if any path is refused nothing is written. The refusal names
the path, the reason, and says so: `error (reason=gate): … ; nothing written`.

`add` never takes an id for the element it creates. Comments, children, lane members:
the system mints the id and echoes `+ $/comments/43s`. The question "how do I add at a
specific id" does not arise because it is not expressible.

`rm` takes any node. Comment removal is `rm dee/comments/43s`; ticket deletion is
`rm dee`, a reversible tombstone that is `set dee/deleted true`, and
`set dee/deleted false` restores it; `rm dee --purge` rewrites it out of the
history. One deletion verb, every kind of node.

### Everything else is a path

`[LAW:dataflow-not-control-flow]`: the four verbs run the same way on every call.
Variability is in the path and the value, never in which verb exists for the case.

```
set dee/status plan                     start
set dee/status impl                     phase --to impl
set dee/status done                     done
set dee/status '{column:dup of:4ou}'    close --resolution duplicate --of
set dee/status backlog                  open
set dee/deleted false                   restore
add /lanes/datetime/claim               start --take
set dee/parent rin                      parent set
add dee/blocks 4ou                      dep add
add rin/children '{title:…}'            new
add rin/children '{title:… children:[{title:…}]}'  new, with its own children
add /children '{title:…}'               new, top level
add dee/plan/reviews '{of:3 how:"…" result:"…" response:"…" verdict:fail}'  verdict, naming the revision it judged
get dee/plan/revisions/3                history --version plan@3
get '(stage=active updated<-6h)'        orphaned
get /lanes                              backlog
get /next                               next
get rin/next                            next, over the lanes whose head is under rin
get /integrity                          doctor
lit2 admin export                       export
lit2 admin import                       import
```

Phases are not a second axis beside status. `plan`, `impl`, `review`, `merge` are
columns in the graph below, so `phase --to impl` is `set dee/status impl` and nothing
else. `next` and `backlog` are derived nodes on the root, like `/integrity`: `/lanes`
read in rank order *is* the backlog, and `/next` is the head the lane rule (below)
selects for this checkout.

A long text field (`body`, `prompt`) is a node. `dee/plan` is a reviewable: two
append-only lists, `dee/plan/revisions`, each a text, and `dee/plan/reviews`, each
naming the revision it judged. `dee/plan` reads the latest revision with its number,
`dee/plan/revisions/3` reads one. `dee/plan/verdict` is derived: the verdict of the
last review naming the latest revision, or nothing. Appending a revision empties the
derived verdict until a review names it.

Values: a bare word, a `"quoted string"` in which `""` is a literal `"` and every
other character is itself, a newline or a backslash included, so a regex or a path
passes through the shell and lit untouched and doubling is the one escape idiom
beside `@@`, `-` for
stdin (once per command; for a list path, one element per line, so a `get` of ids
pipes in), `[a, b]` for a list, `{k:v k:v}` for an object, `@id` for the
addressed node's own id, and inside one `add`, `@N` for the Nth object the value
declares in document order, so `'[{title:A}, {title:B blocks:[@1]}]'` files two
tickets with the edge between them in one call. In prose, `@` before an id or `@N`
is a reference: stored as `@<segments>`, shown as `@` plus the item's current name,
so `"see @2 for the API shape"` in a batch reads `see @template-ids-792a.k2p …` after a
rename and survives being read, edited and set back. `@@` is a literal `@`; any
other `@` is itself, so an email or a decorator needs no escape. A selector or
glob is never a value.

### The column graph is a node

Columns are not built in. The graph lives at `/columns` and is read with the same
verbs; a `set` on it edits the working-tree `.lit/workspace.toml`, and the edit is
in force once `lit config commit` applies it:

```
/columns/backlog     initial:true     next:[plan]     exit:[ready]
/columns/plan        next:[impl]
/columns/impl        next:[review]    entry:[plan/verdict=pass]
/columns/review      next:[merge]
/columns/merge       next:[done]      entry:[pr/verdict=pass]
/columns/done        terminal:true
/columns/dup         terminal:true    entry:[status/of]
/columns/superseded  terminal:true    entry:[status/of]
/columns/obsolete    terminal:true    entry:[status/reason]
/columns/wontfix     terminal:true    entry:[status/reason]
```

"Start" and "done" are not columns. They are edges: start is any transition out of
`initial`, done is any transition into a `terminal`. That is the entire built-in
vocabulary. `terminal` and `initial` are the only flags the engine interprets;
`next`, `entry`, `exit`, and anything the user adds are data the engine validates
against and does not understand. `[LAW:types-are-the-program]`: the engine knows
there is a graph with a start and ends; the user owns the graph.

Reachability follows from `next` alone: a column some `next` names is entered only
along that edge, and a column no `next` names is entered from everywhere. `backlog`
and the terminal columns are in no `next` list, so parking, reopening and closing from
anywhere need no edge of their own. `x/stage` is derived from the flags on the
resolved column: `initial`, `active` (neither flag), or `terminal`, so "in flight" is
one selector key in every dialect.

What follows from `set x/status <s>`, validated against the resolved column graph:

- **claim** is derived. A lane is held by the checkout that made its latest
  establishing transition, out of `initial` or into a `terminal`, on any member; a
  ticket's `claim` reads its lane's. Not settable; `rm <lane>/claim` releases it,
  echoed, `add <lane>/claim` takes it, the establishing act for work left in the
  middle of a column, and parking a member into `initial` ends it. Expiry is `claim_ttl`, resolved
  by nearest ancestor, measured from the holder's newest event on any member.
  `[LAW:no-ambient-temporal-coupling]`: the claim's lifetime is state with an owner,
  not a timer somebody remembers. Every transition echoes its derivations:
  `dee/status backlog → plan   ⇒ dee/claim = this checkout`.
- **a node's stage** is derived from its children, so an epic is finished or
  reopened only through them; a node has no `status`.
- **requirements** are `entry` and `exit` lists on a column, each a test with a
  severity. `impl/entry` requires `plan/verdict=pass`, so `set status impl` is refused
  until a review of the plan's latest revision reads `pass`. `[LAW:single-enforcer]`:
  a requirement is checked in one place, on the transition, against one definition.
- **guidance** is a handler bound `in <column>`. `get dee` prints the ticket and then
  the handler's text, after the data. `[LAW:one-source-of-truth]`: it cannot describe
  a column that does not exist, because it is bound to the column.
- **resolutions** are the terminal columns. `entry:[status/of]` on `dup` is what makes the
  canonical id mandatory; there is no `--of` flag to forget.

### Hierarchy: nearest ancestor wins

Any node may carry a `columns`, `lanes`, or `claim_ttl` child. Descendants resolve by
walking up, the CSS / CLAUDE.md model, per column. One rule, no exceptions:

```
/columns                        default for everything
rin/columns/audit               this epic adds an 'audit' column before impl
```

Path depth is free. Because rank, lane, and claim are all relative to the node they
are resolved in, nothing about the four verbs changes at depth three or depth thirty.
`get '(status=review)'` matches review tickets at every depth, in every subtree's
dialect of review, because the selector matches the node's resolved column, not a
global enum.

## Lanes

The primitive the session reached for without having: the datetime trio (`dee`, `b64`,
`fw9`) is a row — same files, each builds on the last — and the only ways to say so
were rank adjacency (which means nothing) or three dependency edges. "I've got ten
tickets that need to be worked in order" costs an agent twenty calls of edge-stringing
today, and that is why lanes exist.

**A lane is an ordered list node, and it is cross-cutting.** It lives anywhere (the
root, an epic, any node) and references tickets anywhere. Order is the sequence.
A ticket belongs to at most one lane; its `lane` field is derived from membership.

```
/lanes/datetime  = [dee, b64, fw9]
b64/blocked_by   = dee                 derived from position; never stored
```

**A ticket in no lane is its own lane,** at `/lanes/<id>`, so it has a rank path like
any other. Parallel is the default and costs nothing. Serial is one bulk add of ids:

```
add /lanes/datetime '[dee, b64, fw9]'
```

Three tickets, sequenced by the order given, one call. `[LAW:composability]`: the
sequence is a value crossing one boundary, not a family of edge-add calls.

A new lane ranks last. A ticket belongs to at most one lane, so adding it to another
moves it, and the echo shows the removal from the old one.

**Rank is on lanes, not tickets.** A ticket has no rank of its own; it has a position
in its lane, and the lane has a rank. This is what dissolves the question "I'm three
into a lane of five, another epic has ten tickets ranked between my lane's items — do
you serve the lane or the ten?" There is no between. The ten are in their own lane(s),
and those lanes sit above or below as whole units. Reranking a lane moves everything
in it, together, in order, so a rerank can no longer scatter a unit of work across the
backlog. Rank is written as a relation, `set /lanes/datetime/rank '{above: /lanes/help}'`,
`'{below: …}'`, `top` or `bottom`, so two checkouts reranking offline each still mean
what they said when the logs merge. Reordering *within* a lane is one edit on the
list — `set /lanes/datetime '[b64, dee, fw9]'` — echoed as a diff, not N per-ticket
moves.

**Dependencies become the rare cross-cut.** A lane says "these, in this order." A dep
says "that thing, over there, first." Different shapes, different frequency; the
frequent one is now free. The within-epic restriction on edges is lifted, because
sometimes the cross-cut is inside one epic.

**Epics become folders.** Today the epic does three jobs — grouping, ordering, and
lane — and that is why the Unfiled epic had to lie in its description. An epic worked
in sequence is a folder holding one lane. An epic with parallel streams holds several.
The folder has no rank; only its lanes do. `[LAW:escape-local-minima]`: the fused
epic-as-lane is the local minimum the current design settled into one reasonable
step at a time, and it is named here so that it is escaped rather than carried.

**Reading a lane.** `get /lanes/datetime` prints the members in order, one per line,
with each member's status and claim, so an agent standing in a lane sees the whole
row and where it is in it:

```
/lanes/datetime  rank 4   claim this checkout
  dee   impl
  b64   backlog
  fw9   backlog
```

### What `next` serves

One rule, two clauses, no scoring:

> The head of the lane this checkout holds. If it holds none, the head of the
> top-ranked lane.

Commitment is the first clause, priority is the second. Finishing a unit is
structurally favoured because the only way to leave a lane mid-way is to drop the
claim explicitly — a write, echoed — never a side effect of someone reranking a
ticket elsewhere. A solo ticket is a one-element lane, so there is no special case.
Claim-by-construction attaches to the lane: finishing `dee` hands this checkout `b64`
without a second action, and the echo on `set dee/status done` says so.

**Urgent is a priority.** A P0 carries `priority: urgent`, and `/next` serves an
urgent head ranked above the held lane before the held lane's own head. That is the
only interruption the system has.

## Bulk

Bulk edit is almost more common than single edit, comments aside. It falls out of
paths in three escalating forms, none of which is a new verb:

1. **Many paths.** `set dee/priority urgent b64/priority urgent`. Fine for a handful.
2. **Globs.** `add 'rin.*/labels' decompose` — every child of the epic. `get '*/title'`
   across the backlog. `set 'unfiled.*/parent' @id` would mean the node's own id; `@id`
   is the sigil for the node's own id. The session's eight `--lane` calls are not a glob at all:
   `rm /lanes/unfiled` dissolves the lane and every member becomes its own.
3. **Selectors.** `set '(status=backlog labels=bug)/priority' urgent`. A query in parens
   expands to ids and the path continues. This is `ls` and `update` fused, and
   `--dry-run` shows the expanded list before it commits.

## Output

`[LAW:nothing-unseen]` and the CLI binding on stdout semantics: output is a contract.

- Every write echoes the diff it made, one line per path: `dee/status backlog → plan`.
  That is verification and audit log in one line, and it is what a grooming report is
  made of.
- Every derivation a write triggers is echoed with it: `⇒ dee/claim = this checkout`.
- `--dry-run` prints the expanded selector or glob first, then the same lines the
  write would echo, then `(dry run: nothing written)`.
- `get` on one scalar prints the bare value, so it pipes. `get` on many paths or a
  glob prints `path<TAB>value`, one per line, so it can be grepped and fed back to
  `set`; a value holding a newline, a tab, a quote or an edge space prints as a
  quoted string, `""` for a quote and everything else verbatim, and every other
  value bare, so a line splits on its first tab, a quoted value runs to its closing
  quote across as many lines as the body has, and the round trip holds for a body
  with no backslash anywhere for the shell to eat. The read–edit–write round trip
  is the whole agent workflow and it must not need JSON to work; `--json` exists
  for the cases that do, and prints one object keyed by path.
- `get` prints each shared ancestor once.
- Guidance is printed after the node it belongs to, never interleaved with data.

## The table is the schema

The grammar above is the whole `lit --help`, and it fits on one screen because the
surface is small enough to be tabular. The table is not documentation of the CLI; it
is the constraint that forces the CLI to be tabular, and it is the one source the
parser, the help text, the completions, and the quickstart's command section are
generated from. `[LAW:one-source-of-truth]`: help that is derived from the parser
cannot omit `--lane`. What is left for prose is workflow advice — when to transition,
what a good body looks like — and that prose is a handler bound to a column.

A field table completes it: one row per field, with its type, whether it is settable
or derived, and what `set` validates. Absence is visible: if a field is not in the
table, it does not exist. Regularity is visible: a column that is mostly one value
tells you which rows are odd and makes you justify them.

## What is removed

`[LAW:polishing-by-subtraction]`. The current CLI has roughly forty verbs, several with
sub-sub-commands. This design has four verbs, one path grammar, selectors, globs, and
`--dry-run`. Gone, and where each went:

| was | is |
|---|---|
| `start`, `done`, `close`, `open`, `phase` | `set x/status` |
| `verdict --plan <digest> --law …` | `add x/plan/reviews '{of:N …}'`; the review names the revision it judged |
| `history`, `history --version plan@N` | `get x/history`, `get x/plan/revisions/N` |
| `ls --search`, `search` | `get '(text~"…")'` |
| `quickstart <stage>`, `workflows show` | the handler printed after `get x`, `get x/columns` |
| `show --field body`, `--body-file -` | `get x/body`, a `-` value reads stdin |
| `followup --on x` | `add <x's parent>/children {…}` |
| `start --take` | `rm <lane>/claim`, then the transition |
| `comment add` / `comment rm` | `add x/comments` / `rm x/comments/<id>` |
| `label add` / `label rm` | `add x/labels` / `rm x/labels/<name>` |
| `parent set` | `set x/parent` |
| `dep add --from --to --type blocks` | `add x/blocks` |
| `rank --top/--above` | `set /lanes/<lane>/rank top`, `'{above: <lane>}'` (rank is on lanes) |
| `new`, `followup` | `add <parent>/children '{…}'` |
| `children` | `get x/children` |
| `orphaned`, `backlog`, `next`, `ls` | `get '(selector)'` |
| `doctor`, `doctor --fix` | `get /integrity`; each advisory names the write that clears it |
| `workflows edit` | `set /columns/…` edits `.lit/workspace.toml`; `lit config commit` applies it |
| `delete`, `restore` | `rm x`, a reversible tombstone; `set x/deleted false` |
| `archive` | a terminal column: `set x/status '{column:obsolete reason:"…"}'` |
| `purge` | `rm x --purge` |
| `update --lane` | lane membership; `x/lane` is derived |
| `init`, `init --prefix` | `lit admin init`; the prefix is a line in `.lit/workspace.toml` |
| per-verb flag grammars | none; values are values |

Backup, sync, upgrade and the rest of the maintenance surface: real, and not part of
this surface. They belong under a separate `lit admin` table so the everyday one stays
legible.
