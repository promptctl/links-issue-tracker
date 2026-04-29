# Agent-Native Guidance Proposal

Status: draft

Related issues:

- `lit-c5519f81-93c728d5` Agent-native alignment epic
- `lit-c5519f81-da762089` Command outputs: add structured next-step/context injection envelope
- `lit-c5519f81-066a74cb` Quickstart: staged autonomous flow with next-action guidance

Prototype references:

- `51afe8f` Add guidance envelope to agent JSON outputs
- `e4f2768` Stage quickstart guidance flow

These commits are the prototype basis for this proposal and are not required to land with the design-doc PR.

## Summary

This proposal standardizes how agent-facing command outputs describe:

- where the agent is in the workflow
- what the next deterministic actions are
- which compatibility and execution invariants remain true

The current proposal has two layers:

1. an additive JSON guidance envelope for object-shaped command outputs
2. a canonical staged model for `lnks quickstart`, with compatibility fields derived from that stage model

The goal is to make follow-up actions explicit without introducing hidden behavior, silent state transitions, or breaking existing JSON consumers.

## Problem statement

Before this proposal:

- command outputs reported the current result, but did not consistently inject machine-usable context for the next action
- `quickstart` exposed a flat workflow list, but did not represent stages as first-class data
- different commands had no shared contract for workflow boundaries or follow-up guidance
- any future agent guidance risked being reimplemented independently per command

## Goals

- Keep current command result fields canonical.
- Add machine-usable guidance without changing command side effects.
- Make `quickstart` explicitly staged and deterministic.
- Preserve a compatibility path for existing JSON consumers.
- Document all currently unresolved design questions so work can resume later without reconstructing context.

## Non-goals

- Versioning all JSON responses immediately.
- Changing list-shaped JSON outputs such as `lnks ready` in this proposal.
- Adding hidden command chaining or automatic follow-up execution.
- Finalizing the long-term schema for every command family in the CLI.

## Current proposal

### 1. Shared guidance envelope

Object-shaped JSON responses may include these additive top-level fields:

- `workflow_stage`: stable label for the current workflow boundary
- `next_steps`: list of structured next actions
- `invariants`: list of contract statements that remain true for the response
- `trace_ref`: existing command-specific trace reference when a command already produces one

`next_steps` currently use this minimal structure:

```json
{
  "command": "lnks workspace --json",
  "purpose": "Resolve canonical workspace paths and trace locations.",
  "condition": "when a git remote is configured"
}
```

The current merge rules are:

- existing command-specific fields remain unchanged
- guidance fields are additive
- if a payload already defines `trace_ref`, that value remains canonical

### 2. Rollout scope

The current rollout is intentionally limited to object-shaped command responses in the agent loop, such as:

- `workspace`
- `quickstart`
- `sync remote ls`
- `sync fetch`
- `sync pull`
- `sync push`
- `sync status`

List-shaped responses are intentionally unchanged in this proposal.

### 3. Quickstart as a stage model

> **Superseded** by `design-docs/preparing-the-next-loop.md` (2026-04-28). The 5-stage model below is retained as historical context. The current view is that an agent's work loop tangles rather than progressing through ordered stages, and the load-bearing design discipline is preparing the *next* loop rather than steering the current one. See `design-docs/agent-enablement-onboarding.md` for the discovery path.

`quickstart` is modeled as five canonical stages:

1. Session bootstrap/context refresh
2. Work selection
3. Claim/start work
4. Execute with safe mutation guardrails
5. Sync and closeout

Each stage contains:

- a stable `id`
- a human title
- a goal statement
- a list of commands
- a stage-local `next_steps` list

The top-level `quickstart` payload also exposes:

- `workflow_stage`: currently the first stage id
- top-level `next_steps`: currently the first stage next steps
- `workflow`: a compatibility list derived from the stage model
- `examples`: a compatibility command list derived from the stage model

### 4. Compatibility contract

The current compatibility contract is:

- additive fields are safe for existing consumers to ignore
- pre-existing top-level result fields remain canonical
- compatibility fields like `workflow` and `examples` may remain temporarily, but they are derived from the stage model rather than authored independently
- list-shaped outputs are deferred until a versioned compatibility path is agreed on

### 5. Documentation alignment

The current proposal also aligns docs around the same model:

- the CLI reference describes the additive guidance envelope
- the agent workflow guide mirrors the same five quickstart stages

## Rationale

### Why additive fields instead of a wrapper object?

Because existing consumers already parse current top-level fields, and an immediate wrapper such as:

```json
{
  "data": { ... },
  "guidance": { ... }
}
```

would force a breaking migration across existing scripts and tools.

### Why only object-shaped outputs?

Because object-shaped outputs can accept additive fields without changing their fundamental JSON shape.

List-shaped outputs cannot gain top-level metadata without either:

- breaking the output schema
- or introducing a wrapper object

That should be handled only with an explicit versioning and migration plan.

### Why make `quickstart` stages canonical?

Because the alternative is multiple independent representations:

- staged JSON
- flat `workflow`
- `examples`
- text rendering

Keeping stages canonical and deriving the other forms reduces drift and makes later edits local.

## Current limitations

- Guidance is not yet present on list-shaped outputs.
- `next_steps` use a small schema and may not be rich enough for future orchestration.
- `workflow_stage` currently names the current command boundary, but there is no separate notion of recommended next stage beyond `next_steps`.
- The proposal does not yet define whether all future command families should adopt the same rollout strategy.
- The proposal documents the merge-to-`master` closeout boundary, but broader workflow-state questions remain outside this doc.

## Explicit open questions

These are intentionally unresolved.

1. Should the long-term JSON contract remain additive, or should the CLI eventually move to a versioned wrapper such as `data + guidance`?
2. Should `next_steps` stay as `{command, purpose, condition}`, or should it grow fields such as `kind`, `requires_confirmation`, `machine_only`, or `produces`?
3. Should list-shaped outputs stay unchanged indefinitely, or should they move to a versioned wrapper in a later migration?
4. Should `workflow_stage` remain a simple current-boundary label, or should the payload also expose a separate canonical `recommended_next_stage`?
5. Should `quickstart` remain opinionated about closeout semantics in the stage model, or should those details be delegated entirely to other docs and command-specific outputs?
6. Should the quickstart stage model include non-command notes as first-class structured data, or should stage guidance remain command-only plus goal text?
7. Which additional command families, if any, should adopt the guidance envelope next?
8. Should `trace_ref` remain a command-specific optional field, or should the guidance contract define stronger rules for when it must appear?
9. Should the docs continue mirroring the quickstart stage model directly, or should one doc become the sole narrative source and the others summarize it?
10. If we later adopt a richer workflow-state model, should `quickstart` stages map directly to those states or remain a separate instructional layer?

## Resume checklist

When this work resumes, start with:

1. Confirm whether the additive-envelope direction is still preferred.
2. Decide whether `next_steps` need a richer schema.
3. Decide whether list-shaped outputs should remain unchanged or move to a versioned wrapper.
4. Reconcile the quickstart stage model with any newer workflow-state proposals.
5. Close or update the related issues only after the preferred direction is confirmed.
