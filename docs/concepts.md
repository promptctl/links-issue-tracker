# Core concepts

## One workspace identity per clone

`links` stores its state under:

```txt
$(git rev-parse --git-common-dir)/links/
```

That means all worktrees in the same clone share one up-to-date issue view.

## One write boundary

All writes happen through `lnks`. You do not edit database state directly.

## Relationships are first-class

Issues can be linked as:

- `blocks`
- `related-to`
- `parent-child`

## Lifecycle is explicit

Issue lifecycle transitions (`open`, `close`, `archive`, `delete`, etc.) require reasons and are recorded in history.
