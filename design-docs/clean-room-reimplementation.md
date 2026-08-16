# Clean-room reimplementation: how lit replaces a licensed component with code it owns

Status: accepted design, normative (2026-08-15). This document governs how the
epic `links-licensing-c0ce` replaces licensed components. Where it and a ticket
in that epic disagree, **this document wins and the ticket is wrong** — the same
standing [work-claims.md](work-claims.md) has for the claims epic. The archive's
usual "when a design doc and the code disagree, the code wins" tiebreak does not
apply here: this is a protocol for producing code, not a description of code
that exists.

## Summary

`links-licensing-c0ce` removes LGPL, MPL, and unclassifiable licenses from what
lit ships by replacing the components that carry them. That replacement is only
worth what its provenance is worth. A reimplementation nobody can prove is
independent is **worse** than the dependency it replaced: it trades an honest,
documented MPL-2.0 row for an MIT assertion we cannot substantiate, inside an
SBOM whose entire purpose is being believed.

The audience is the enterprise license review the whole epic serves, and its
question is: *how do you know this is not derived from the original?* The answer
has to be a process with a record, not a recollection. This document is that
process.

Four agents, in sequence, each one fresh. The first reads the original and
writes down only what it observably does. The second strips that description of
anything that says *how*. The third turns the stripped description into
implementation tickets. The fourth writes the code, inside a sandbox where the
original cannot be opened. Only the reviewed description crosses the first
boundary; only the reviewed tickets cross the second. The wall is not a promise
anybody makes — it is the fact that the later agents were never given the thing.

## The standing requirement

**Everything this epic produces is promptctl's own work, under promptctl's own
copyright.** Not "licensed to us." Not "permitted for us to use." Ours.

No file we hold out as our own carries another author's material — not a
function, not a constant table, not a test fixture, not a doc comment. Any
proposal that ends with someone else's authored bytes inside a file we claim is
a proposal that has already failed, whatever license permits it.

Every other rule in this document exists to serve that one. When a rule below
seems ambiguous, resolve it toward this sentence.

## Cut before you replace

Before designing any replacement, check whether anything calls the dependency at
all. If nothing does, delete what pulls it in and stop.

`links-licensing-c0ce.6` is the worked example. `kch42/buzhash` looked like a
copy-or-rewrite decision until someone traced its callers: dolt's
`rollingHashSplitter` is the only one, and it is reachable solely from a
benchmark — the live `keySplitter` never touches it. So the answer was neither
copy nor rewrite but deletion. Nothing was copied, nothing was written, and the
result is no attribution obligation, no license text to reproduce, and no new
code to own or maintain forever.

Cutting outranks both grounds below whenever it is available, because it is the
only option that leaves the codebase smaller than it found it.

## The default is rewrite

When lit replaces a component to escape its license, the replacement is written
through this clean room. That is the plan of record. **An agent does not need to
justify following it** — the justification is already here, and re-deriving it
per component is how the protocol gets talked out of.

There are exactly two grounds for *not* rewriting, and neither one is a legal
opinion we formed about someone else's code.

**Ground 1 — the license expressly grants the copy.** The copyright holder
handing us permission in writing, not us concluding permission was unnecessary.

*Nothing in this epic relies on this ground.* buzhash's WTFPL would have
qualified, and `links-licensing-c0ce.6` cuts the dead code instead, which is
strictly better: an express grant still leaves another author's bytes inside a
file we call ours, and the standing requirement forbids that. Treat any appeal
to this ground as needing the owner's sign-off.

**Ground 2 — we already own equivalent code**, written for our own reasons
before this epic existed. `internal/filelock` is the case: it was written for
the snapshots epic (`links-snapshots-3dtv`), to prove producer liveness through
the kernel when collecting snapshot residue. Nobody was thinking about fslock's
license when it was built. `links-licensing-c0ce.4` substitutes it for fslock,
and that copies nothing because there is nothing being copied from.

Note what makes ground 2 legitimate: the **motivation** was independent, not
merely the calendar. Code written last week to escape a license is a rewrite and
goes through the protocol, no matter how convenient it is to reclassify. Code
written for an unrelated problem that happens to fit is ours already.

One consequence of ground 2 worth stating plainly, because the adapter is where
it leaks: when substituting `internal/filelock` for fslock, **do not read
fslock's source while writing the adapter.** Derive the contended-lock semantics
you need from dolt's call sites, which are Apache-2.0 and ours to read under the
fork.

## What is never a ground

**Our own conclusion that the material is unprotectable.**

You will reach a component where the rewrite looks like ceremony, and the
reasoning that gets you there will be good. It runs something like: this is a
table of constants the format dictates, or these field names are fixed by the
wire protocol, or any competent engineer writes this the same way — so there is
no protected expression here, so copying it is not copying anything anyone could
own, so the rewrite is ritual and the honest engineering call is to skip it.

Every step of that may be correct as law. It is still forbidden, and the reason
has nothing to do with whether you are right.

Merger, scènes à faire, thin originality — these are **defenses**. A defense is
something you deploy in an argument. This epic exists so that the argument never
happens: so that a reviewer reading lit's SBOM finds nothing to stop on and asks
nothing. Acting on a defense means the provenance record now rests on our
reading of copyright doctrine instead of on a process, and a reviewer who
disagrees with that reading has found the exact question the epic promised would
not exist. We do not get to unilaterally declare that someone else's license does
not apply to us, however confident we are that a court would agree.

The tell is the quality of your own reasoning. Laziness announces itself; this
does not. It arrives sounding like rigor, with doctrine behind it, and it feels
like the senior judgment call — seeing past the ceremony to what actually
matters. An earlier draft of this epic's own ticket made that argument at length
and made it well, and the owner rejected it. So if you have just built a careful
case for why *this particular component* does not need the protocol, you have not
found the exception. You have re-derived the rejected argument, and the care you
put into building it is the evidence, not the excuse.

Rewrite it.

If the rewrite is hard because the spec is thin, that is a stage-1 defect, and
the fix is another observation round behind the wall — never a peek.

## The four roles and the wall

Each stage is a **fresh agent**. Agents do not move down the chain: an agent that
has read a stage's inputs is finished when that stage is, and the next stage
starts with someone who has read nothing.

| Stage | Ticket | May read | Produces | Barred from |
|---|---|---|---|---|
| 1 — dirty room | `.11` | the original, freely | a behavioral spec: what the component observably does | every later stage, permanently |
| 2 — review | `.12` | the draft spec only | the reviewed spec, with every *how* removed | stages 3 and 4 |
| 3 — ticket-writing | `.13` | the reviewed spec only | implementation tickets | stage 4 |
| 4 — implementation | `.14` | the tickets only | the replacement code | — |

Stage 2 is barred from later stages for a reason that is easy to miss: its whole
job is to find sentences in the draft spec that describe implementation, which
means that by the time it has done its job, it has read them. The reviewer is
contaminated *by reviewing*. That is expected and costs nothing, because the next
stage is another fresh agent.

The wall between these stages is not discipline and not a promise. It is the
plain fact that a fresh agent inherits no context, so stages 2 through 4 were
never given the original in the first place. The only thing discipline has to
carry is the two crossings below and the sandbox in stage 4.

## What crosses

Only two artifacts cross, and each gets reviewed before it does.

**Boundary 1 — out of the dirty room: the behavioral spec, and nothing else.**
Notes taken while reading the original, the dirty-room agent's transcript, quoted
snippets, "here is roughly how it works" summaries — all stay behind the wall.

**Boundary 2 — into implementation: the reviewed tickets, and nothing else.**

Read that second one carefully, because it is the crossing people forget. The
tickets are a contamination vector exactly like the spec. A ticket that prescribes
a structure — *"parse the header as a 4-byte little-endian length prefix"* —
breaches the wall just as surely as a spec sentence saying it. That it arrived in
a ticket rather than a document changes nothing about where the knowledge came
from.

(That example is deliberately drawn from an unrelated component. This document is
read by stage-3 and stage-4 agents, so an illustration taken from the thing they
are about to reimplement would be the exact leak it is warning about. The same
care applies to every ticket, review note, and commit message in this chain.)

Both crossings get the same review, against the same test: **does any sentence
here describe how the original works, rather than what the replacement must do?**

## The isolation mechanism

Context isolation is the easy half — a fresh agent inherits nothing. Filesystem
isolation is the half that needs work, because the originals are already unpacked
on the machine. On this Mac, `github.com/hashicorp/golang-lru` sits in the Go
module cache as 12 readable `.go` files, and a single `find` reaches it.

**Nothing goes in this section that was not tested by trying to defeat it.** What
follows is measured, not reasoned.

### The probe

[`scripts/cleanroom-reach-probe.sh`](../scripts/cleanroom-reach-probe.sh) — six
routes an agent takes to a module's source, run inside each candidate
environment. It exits 0 only when every route is blocked, so it works as a gate
and not just a report. Always run a control pass outside the mechanism too: a
route that fails everywhere proves nothing about the sandbox.

```
scripts/cleanroom-reach-probe.sh                                          # control
scripts/cleanroom-sandbox.sh --offline scripts/cleanroom-reach-probe.sh   # candidate
```

The six routes:

1. the module cache by absolute path — the path an agent types from memory
2. a search by name, for the agent who knows the name but not the path
3. opening a source file
4. `go doc`, which prints the original author's doc comments
5. `go mod download` into a relocated cache, which defeats a merely-emptied one
6. `curl` of the module zip from `proxy.golang.org`, bypassing the go command

### Results

| Route | No isolation | Standalone repo | Seatbelt, offline | Seatbelt, network allowed |
|---|---|---|---|---|
| 1 module cache, absolute path | **reached** (12 files) | **reached** (12 files) | blocked | blocked |
| 2 search by name | **reached** | **reached** | blocked | blocked |
| 3 open a source file | **reached** (6715 B) | **reached** (6715 B) | blocked | blocked |
| 4 `go doc` | **reached** (1567 B) | blocked | blocked | blocked |
| 5 `go mod download` refetch | **reached** | **reached** | blocked | blocked |
| 6 `curl` the module zip | **reached** (29453 B) | **reached** | blocked | **reached** (29453 B) |

**The standalone repository fails.** The epic named it as a leading candidate —
"develop the replacement in a standalone repository whose go.mod never requires
the original, so nothing in a normal build fetches or surfaces it." Measured, it
closes exactly one route of six. It stops the build from *surfacing* the
original; it does nothing to stop an agent from *going and getting it*, which is
the thing that actually happens. Keep it as build-graph hygiene. Do not call it
isolation.

**Containers were not tested.** `podman` is installed but has no VM on this
machine, so nothing local can run one. A Linux container with the module cache
masked would plausibly hold, and CI is where that could be established. Until
someone runs the probe inside one, it stays an untested candidate.

**A remote agent environment was not tested either.** It would arrive without
this machine's module cache, closing routes 1–3 by construction, but it has
network, so route 6 stays open exactly as in the last column above.

### The mechanism that holds

macOS seatbelt, via `sandbox-exec`, with the module cache denied — wrapped as
[`scripts/cleanroom-sandbox.sh`](../scripts/cleanroom-sandbox.sh):

```
scripts/cleanroom-sandbox.sh [--offline] <command> [args...]
```

It generates the profile from `go env GOMODCACHE` at run time:

```lisp
(version 1)
(allow default)
(deny file-read* (subpath "<resolved GOMODCACHE>"))
(deny file-read* (subpath "<resolved GOPATH>/pkg/sumdb"))
(deny network*)          ; --offline only; an agent needs its own API — see below
```

Beyond the six routes, six escape attempts were run inside it. All were held,
and a control run outside the sandbox confirms each one genuinely works when
unconstrained — an escape that fails everywhere proves nothing:

| Escape attempt | Outside sandbox | Inside sandbox |
|---|---|---|
| the APFS firmlink path, `/System/Volumes/Data/Users/…` | **reached** | held |
| `..` traversal around the denied prefix | **reached** | held |
| a nested `sandbox-exec` with a permissive profile | **reached** | held |
| `cp -R` the tree out into an allowed directory | **reached** | held |
| `git clone` the upstream repository | **reached** | held |
| relocating `GOMODCACHE` and refetching | **reached** | held |

A child process cannot widen an inherited profile, which is what kills the
nested-`sandbox-exec` escape and most variations on it.

### Two traps, both found by testing

**A profile must name resolved physical paths.** A rule written against a
symlinked path silently enforces nothing — it does not error, it does not warn,
it simply never matches. Denying `/tmp/x` on macOS protects nothing, because the
real path is `/private/tmp/x`; denying `/private/tmp/x` blocks access through
*both* spellings. This was found by writing the wrong one and watching a canary
file come straight back out of a sandbox that was supposed to deny it.

The wrapper closes this trap by construction — it derives the path from
`go env GOMODCACHE` and resolves it with `pwd -P` before writing the rule, so a
hand-copied path cannot drift and a symlinked one cannot be expressed. Anyone
writing a profile by hand instead inherits the trap. Either way, the profile is
not self-evidently correct: run the probe.

**Sandbox the agent process, not its shell commands.** Wrapping individual `bash`
calls in `sandbox-exec` is theater: an agent's file-reading tools run inside the
harness process, not in the shell it spawns, so they never enter the profile at
all. Verified directly — a canary file that a sandboxed shell was refused was
read without complaint by the harness's own file-read tool a second later.
Launching the whole agent process under the profile does work: seatbelt denies at
the syscall level, so an in-process `open()` is refused exactly like a spawned
`cat` is.

### The residual, stated honestly

An agent needs network to reach its own API, so stage 4 cannot use `--offline`.
Without that flag route 6 reopens: `curl` pulls the module zip.

That residual is acceptable, and the reason is the shape of the two failures,
not their likelihood. An unpacked cache at a guessable path gets reached **by
accident** — a wide `grep`, a stack trace, a `go doc` that resolves further than
expected. The sandbox closes every accident. Fetching a module that appears in no
ticket, in no `go.mod`, and in nothing the agent was told about is not an
accident; it is a decision, and decisions are what the contamination rule
governs. The wall's job is to make the original unreachable by accident. The rule
below handles the rest.

If a future stage-4 setup runs somewhere with an egress allowlist — a container
or CI runner permitting only the API endpoint — that closes route 6 too, and it
should be preferred when available.

## The contamination rule

**When an agent at stage 2, 3, or 4 reads the original, that agent's work product
is discarded and the stage restarts with a fresh agent.** Not reviewed. Not
salvaged. Discarded.

This holds however it happened: deliberately, or through a stack trace, a
`go doc` dump, a search result, a helpful paste. Intent does not enter into it,
because the harm does not depend on intent — the knowledge is in the context
either way.

The moment this rule has to survive is a stage-4 agent at hour three with a
failing test, thinking: *I'll just glance at the reference implementation to see
what it does, then write my own version from memory.* That glance costs the epic
its deliverable and **leaves no trace**. The code still compiles. The tests still
pass. `tools/licenses` still prints MIT. The provenance claim is now false and
nobody — not the reviewer, not the owner, not the next agent — has any way to
find out. There is no later check that catches this. This rule is the only thing
standing there.

So the redirect, when you feel it: the spec was incomplete. That is a stage-1
defect, and the fix is another observation round behind the wall by an agent who
is already burned. It is not a peek.

**Recovery is cheap, which is why the rule can afford to be absolute.** A
contaminated agent is not a disaster to be managed — it is a session to be
ended. Stop, write the handoff (`/message-in-a-bottle`), and start a fresh
session that has read nothing. The cost is one context window. Weigh that against
a false provenance claim in a published SBOM, and there is no decision to make:
declare the contamination and restart. An agent that hides a slip to avoid
restarting has done far more damage than the slip.

## Component dispositions

Recorded so no agent re-litigates them.

**`hashicorp/golang-lru` (MPL-2.0) — full chain.** A genuine reimplementation
with wide expressive room and no grant of any kind. The archetype this protocol
was written for, and the only component that runs all four stages.

**`go-sql-driver/mysql` (MPL-2.0) — rewrite, no chain.**
`links-licensing-c0ce.2` writes the error type it needs without reading the
original. lit touches this dependency at two call sites, both ours; the type is
small and the discipline costs nearly nothing. That its shape is largely dictated
by the MySQL wire protocol is not a license to copy it — see *What is never a
ground*, which was written with this component in mind.

**`kch42/buzhash` (WTFPL) — cut.** `links-licensing-c0ce.6` deletes dolt's
`rollingHashSplitter`, the only caller, reachable solely from a benchmark.
Nothing copied and nothing written, so there is no attribution obligation, no
profane license text in `THIRD_PARTY_LICENSES`, and no new code to own.

**`dolthub/fslock` (LGPL-3.0) — nothing is copied, under ground 2.**
`links-licensing-c0ce.4` substitutes `internal/filelock`. Do not read fslock's
source while writing the adapter; derive the semantics from dolt's call sites.

Substituting a permissive (Apache-2.0 / MIT) third-party library for the LRU
instead of rewriting it is **a change of plan, not an agent's call**. The default
is the chain. If a survey turns up a compelling candidate, record it and take it
to the owner — do not cancel the chain on your own judgment.

## The owner stays uncontaminated

The owner will not read the original source, the spec, or the resulting
implementation. That is a deliberate part of the wall, not a lack of interest.

It has a hard consequence for every ticket in this chain: **no acceptance
criterion may depend on the owner reading any of those.** Every gate is
agent-verifiable or machine-checkable, or it is not a gate. "The owner reviews
the implementation" is not an acceptance criterion available to this epic.

This document is not one of the contaminating artifacts — it describes the
protocol, not the component — so the owner can read it freely.

## Open edges

**Containers and remote environments are untested candidates.** Both are
plausible and neither has been run against the probe. The probe script is the
thing to run; a mechanism nobody tried to defeat does not go in this document,
and that applies to future additions as much as to the ones above.

**Egress allowlisting would close the last route.** If stage 4 ever runs
somewhere that can permit the API endpoint and nothing else, route 6 closes and
the residual disappears. Worth taking when the environment offers it; not worth
building bespoke.

**The wrapper is macOS-only.** `scripts/cleanroom-sandbox.sh` requires
`sandbox-exec` and refuses to run without it, pointing at the container path
instead. Linux stage-4 work needs the container candidate settled first, and the
probe is what settles it.
