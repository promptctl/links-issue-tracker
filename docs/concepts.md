# Core concepts

## One workspace identity per clone

`links` stores its state under:

```txt
$(git rev-parse --git-common-dir)/links/
```

That means all worktrees in the same clone share one up-to-date issue view.

## One write boundary

All writes happen through `lit`. You do not edit database state directly.

## Every ticket lives inside an epic

A task, bug, feature or chore always has a parent, and every parent is an epic, so only
an epic may be top-level, on its own or inside another epic. The rule is enforced where
the hierarchy is written, so a ticket without a home cannot be created, reparented into,
or detached into existence. A ticket therefore always has a checkpoint it serves, ranks
among its epic's children rather than against the epics, and "the other tickets in its
epic" are its siblings.

## Relationships are first-class

Issues can be linked as:

- `blocks`
- `related-to`
- `parent-child`

## Lifecycle is explicit

Issue lifecycle transitions (`open`, `close`, `archive`, `delete`, etc.) require reasons and are recorded in history.
