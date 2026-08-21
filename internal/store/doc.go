// Package store owns lit's workspace state in an embedded Dolt database:
// opening and closing it, mutating it under locks, and minting every
// lit-owned lock path.
//
// # The lock discipline
//
// This package doc is the one home of lit's lock discipline — the rules
// every coordination point on a workspace's Dolt directory follows. The
// kernel flock primitive itself lives in
// github.com/promptctl/primitives/filelock (moved out of this repository's
// internal/filelock under links-licensing-c0ce.4, kernel-liveness contract
// intact); the discipline is lit's own and stays here, beside the package
// that mints the lock paths and stamps the lock meanings, so an agent adding
// a coordination point finds one pattern to copy and one place to put the
// file. [LAW:one-source-of-truth]
//
// ONE PRIMITIVE. Owner exclusion — any point where a holder must exclude
// others across a window of time — is an flock through filelock.Acquire, and
// "is the owner still alive" is answered only by exclusive acquisition,
// never by an mtime, a PID probe, or a wall-clock threshold. The kernel's
// answer is right on every death mode; every heuristic has been measured
// evicting a live holder (a commit-lock file backdated eleven minutes let a
// second process's mutation walk past its running owner and exit 0). Each
// lock declares its retry budget at its own call site, and contention
// travels as Acquire's acquired=false value, given its domain meaning at
// each caller's own boundary — a store sentinel, a collector's deliberate
// silent skip, a mirror's coalesce. Name allocation is not owner exclusion —
// the trace file's O_EXCL retry claims a unique name and holds nothing, and
// the snapshot slot's os.Mkdir reservation, though held across the copy
// window, has its owner's liveness proven by the beacon it sits under — so
// neither carries a liveness question of its own and both stay off this
// primitive. Owned state is not owner exclusion either: the mirror-pending
// marker's existence carries "a mirror is owed" and stays a plain file,
// while the separate liveness question ("is that mirror still coming") rides
// the mirror beacon's flock — one file per fact, because removing a marker
// an old binary also deletes from under a live flock would split the lock
// across two inodes (the commit lock's rename lesson).
//
// ONE ACQUISITION ORDER, outermost to innermost:
//
//	workspace → Dolt's own .dolt/noms/LOCK → commit → snapshot producer beacon
//
// A holder of an inner lock never waits on an outer one. Two entries need
// spelling out, because no lit call site shows them:
//
// Dolt's LOCK is ordinarily acquired by the embedded driver, not by lit: an
// engine takes it when it opens and holds it until it closes. A write
// engine opens eagerly inside openStoreConnection — before any commit lock
// — and refuses Dolt's read-only fallback, retrying its open boundedly, so
// a live write Store stands at "holds LOCK, takes commit per mutation" for
// its whole lifetime. A read engine opens lazily at first SQL and never
// waits on LOCK (a 100ms attempt, then the read-only fallback), so it
// contributes no wait edge wherever it opens — and the laziness is
// deliberate: a reader that opened eagerly under a transient holder would
// be permanently read-only, while a lazy one opens after any commit-lock
// wait, holder gone, write-capable for auto-migration. lit acquires it
// directly in exactly one place — the snapshot copy's
// LockDoltJournalExclusive, which must exclude engine-lifecycle I/O without
// opening an engine. That standing Store hold is the trap in the natural
// reading of "hold Dolt's LOCK during a walk": taking LOCK (opening a write
// engine, or locking the file directly) while holding the commit lock
// inverts the order against every live write Store. Take it before commit
// or not at all. One deviation is tolerated, not copied: a GC-contention
// retry rotates the store's connection mid-mutation, re-acquiring LOCK
// under the held commit lock. It cannot wedge — the re-open's wait is
// bounded (~30s, engineOpenRetryMaxElapsed) strictly inside every
// commit-lock waiter's ~15-minute budget, so the inverted edge always
// breaks by the re-open failing the mutation loudly — and the bound is the
// tolerance's whole justification; see Store.reconnect. (The one write
// engine outside the Store lifecycle, the adopt clone's, runs under the
// exclusive workspace hold and never takes the commit lock.)
//
// lit once minted a second lock for the "one write-capable engine per path"
// fact — .links-engine.lock, taken by write opens but not by OpenForRead.
// That partial shadow of LOCK is retired (links-locking-il18.3): every
// engine open contends on LOCK itself with a bounded retry, and the name
// went with the mechanism — minting a lock beside LOCK again, under any
// name, recreates the two-representations disagreement.
//
// The sync-push lock sits outside the slots: its holder goes on to take
// everything in them (the mirror cycle opens a full write Store), but every
// acquisition of it is a non-blocking probe (maxAttempts 1), so no process
// ever waits ON it — and a lock with no inbound wait-edge cannot complete a
// cycle.
//
// The mirror liveness beacon sits outside the slots too, and is the
// outermost acquisition in practice: every answerer for the mirror-pending
// marker holds it SHARED — the claimant from its claim until its obligation
// ends (released together with a claim it un-claims; abandoned to process
// exit once its mirror is spawned), and the mirror from that mirror's entry
// (before the sync-push lock, before any engine), overlapping lifetimes —
// so acquiring it exclusively is kernel proof nobody answers, and a
// two-step probe (shared first, exclusive as the deciding last step)
// further tells answerers from an exclusive squatter. Every acquisition
// happens holding nothing: probes are non-blocking (maxAttempts 1, released
// the instant they succeed), and the shared takes retry only against a
// probe's microsecond window (~1s budget). No inbound wait-edge from any
// inner holder exists, so it cannot complete a cycle either.
//
// ONE HOME. A lock file sits beside the dolt directory — at
// dirname(databasePath), the position every lit-minted *LockPath helper in
// this package mints — so a `lit snapshots restore` that rotates the dolt
// directory cannot move the lock out from under its acquirers. An exception
// states its reason at the site that mints the path; there are three: the
// snapshot producer beacon lives inside snapshots/ with the artifacts whose
// liveness it proves; the adopt-pending marker (a condemnation record, not a
// lock) lives inside the dolt root precisely so the rotation the locks must
// survive carries the marker with the directory it describes; and Dolt's own
// journal LOCK lives inside the dolt directory because it is Dolt's file,
// guarding that directory's journal wherever the directory goes (see
// DoltJournalLockPath).
package store
