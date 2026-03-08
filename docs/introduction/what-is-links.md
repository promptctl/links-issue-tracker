# What is links?

`links` is an issue tracker that lives inside your existing Git repo and shares one database across all worktrees in that clone.

The design goal is simple:

- keep issue state local and fast
- keep sync explicit and deterministic
- keep automation predictable for agents

`links` uses Dolt as its storage backend so every issue mutation becomes a committed database change. You work with issues through one CLI (`lit`) and sync those DB commits through your existing Git remotes.

If you already think in Git workflows, `links` keeps that mental model:

- issues change locally first
- local state is committed by default
- remote sync is pull/push
- conflict handling is explicit
