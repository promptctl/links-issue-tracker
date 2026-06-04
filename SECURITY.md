# Security Policy

`links` (`lit`) is local-first software: it persists your issue data in an
embedded [Dolt](https://www.dolthub.com/) database and reads from and writes to
your git working tree. Because it touches your repository state directly, we
take security reports seriously and want to hear about anything that could let
one user's input compromise another's data, escalate file-system access beyond
the invoking user, or corrupt the stored backlog.

## Reporting a vulnerability

**Please do not open a public GitHub issue for security reports.**

Report privately through either channel:

- **GitHub** — use [Private vulnerability reporting](https://github.com/promptctl/links-issue-tracker/security/advisories/new)
  on this repository (Security → Report a vulnerability).
- **Email** — `brandon.fryslie+lit-security@gmail.com`.

Include enough detail to reproduce: the `lit` version (`lit version`), your OS,
the commands run, and what you observed versus expected. A minimal repro repo or
command sequence helps us confirm fast.

## What to expect

- **Acknowledgement within 3 business days** that we received the report.
- **An initial assessment within 7 business days** — whether we can reproduce it,
  our severity read, and the likely fix path.
- We'll keep you updated as we work a fix, and credit you in the release notes
  when the fix ships (unless you'd prefer to stay anonymous).

Please give us a reasonable window to ship a fix before any public disclosure.

## Versions in scope

Fixes land on the current `master` branch and the latest tagged release. Older
tags are not separately patched — upgrade to the latest release to receive
security fixes.

## Out of scope

`lit` runs with the privileges of the user who invokes it, against repositories
that user already controls. The following are expected behavior, not
vulnerabilities:

- Anything that requires local write access the attacker already has (e.g. an
  actor who can already edit your working tree or the `links/` Dolt directory
  can already change your backlog — that's the trust model, not a flaw).
- Reading or modifying data in a repository the invoking user can already read
  or modify by hand.
- Denial of service that requires feeding `lit` a deliberately malformed local
  repository you control.

If you're unsure whether something is in scope, report it anyway — we'd rather
triage a non-issue than miss a real one.
