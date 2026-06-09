# Design docs

This directory is a public archive of the design thinking behind `links` (the
`lit` CLI) — audits, refactor plans, and proposals written while building the
tool, kept in the open so anyone evaluating the project can see *how* it was
reasoned about, not just the result.

## How to read these

These documents are working artifacts, not reference documentation. Each one
carries its own `Status:` header that tells you how to treat it:

- **Living** — actively maintained and still authoritative (e.g. the project's
  north star).
- **Snapshot** — a dated point-in-time analysis. Accurate as of its date; the
  code has moved on since, so expect divergence from the current tree.
- **Draft / proposal** — a direction that was being explored. Some of it landed,
  some didn't.

When a design doc and the code disagree, **the code wins** — these are the
record of intent, not a spec the implementation is held to. For current usage,
see the [README](../README.md) and the [docs](../docs/) site.

## What's here

- **[project-intent.md](project-intent.md)** — the north star: what this project
  is for and the design stance behind it. *Living document.*
- **[REFACTOR_PLAN.md](REFACTOR_PLAN.md)** — the decomposition plan for the
  `cli.go` / `store.go` god-files and the absorbed-variance work.
- **[COMPLEXITY_AUDIT-2026-04-19.md](COMPLEXITY_AUDIT-2026-04-19.md)** — a
  point-in-time complexity audit of the source tree.
- **[preparing-the-next-loop.md](preparing-the-next-loop.md)** — the architecture
  of agent enablement: making the tool drive the next unit of work.
- **[agent-enablement-onboarding.md](agent-enablement-onboarding.md)** — the
  discovery path an agent follows to get productive in a fresh repo.
- **[agent-identity-and-ownership.md](agent-identity-and-ownership.md)** — how
  agents are identified and how ticket ownership is modeled.
- **[agent-native-guidance-proposal.md](agent-native-guidance-proposal.md)** —
  a proposal for injecting workflow guidance into command output.
