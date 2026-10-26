// Package store owns lit's workspace state in an embedded Dolt database:
// opening and closing it, mutating it under locks, and minting every
// lit-owned lock path but the config lock and the backups lock (see below).
//
// # The lock discipline
//
// This package doc is the one home of lit's lock discipline — the rules
// every coordination point on a workspace's Dolt directory follows. The
// kernel flock primitive itself lives in
// github.com/promptctl/primitives/filelock; the discipline is lit's own and
// stays here, beside the package that mints the lock paths and stamps the
// lock meanings, so an agent adding a coordination point finds one pattern to
// copy and one place to put the file. [LAW:one-source-of-truth]
//
// ONE PRIMITIVE. Owner exclusion — any point where a holder must exclude
// others across a window of time — is an flock through filelock.Acquire, and
// "is the owner still alive" is answered only by exclusive acquisition,
// never by an mtime, a PID probe, or a wall-clock threshold. The kernel's
// answer is right on every death mode; every heuristic can evict a live
// holder. Each lock declares its retry budget at its own call site, and
// contention travels as Acquire's acquired=false value, given its domain
// meaning at each caller's own boundary — a store sentinel, a collector's
// deliberate silent skip, a mirror's coalesce. Name allocation is not owner
// exclusion — the trace file's O_EXCL retry claims a unique name and holds
// nothing, and the snapshot slot's os.Mkdir reservation, though held across
// the copy window, has its owner's liveness proven by the beacon it sits
// under — so neither carries a liveness question of its own and both stay
// off this primitive. Owned state is not owner exclusion either: the
// mirror-pending marker's existence carries "a mirror is owed" and stays a
// plain file, while the separate liveness question ("is that mirror still
// coming") rides the mirror beacon's flock — one file per fact, because
// removing a marker an old binary also deletes from under a live flock would
// split the lock across two inodes.
//
// ONE ACQUISITION ORDER, outermost to innermost:
//
//	workspace → bootstrap → Dolt's own .dolt/noms/LOCK → commit → snapshot producer beacon
//
// A holder of an inner lock never waits on an outer one. The bootstrap lock
// is held only inside ensureDoltDatabase, by an opener that holds the
// workspace lock and no engine yet; the engines its holder opens are on a
// staging path no other process opens, so it waits on no lock any other
// holder takes. Two entries need spelling out, because no lit call site
// shows them:
//
// Dolt's LOCK is ordinarily acquired by the embedded driver, not by lit: a
// write engine takes it when it opens and holds it until it closes. A write
// engine opens eagerly inside openStoreConnection — before any commit lock
// — and refuses Dolt's read-only fallback, retrying its open boundedly, so
// a live write Store stands at "holds LOCK, takes commit per mutation" for
// its whole lifetime, but for one gap: a rewrite of its history, a follow of
// a re-rooted remote (reroot_follow.go) or the history bound's own re-root
// (history_bound.go). Each runs with no commit lock held and through
// withEngineClosed, which closes the engine to give up LOCK, retakes it
// through holdForRefWrite to rewrite the branch, and reopens. Another writer
// can take LOCK inside that gap; the rewrite and the reopen then wait for it
// exactly as any open does, and a reopen that cannot get LOCK fails loudly,
// its closed engine failing every later query.
// A read engine opens eagerly too, read-only, and never tries
// LOCK, so it contributes no wait edge anywhere and keeps no writer out — and
// a read Store takes no commit lock at all: its schema check is answered with
// reads, and a workspace whose schema trails the binary is handed to Open, so
// a reader never applies DDL and being read-only costs it nothing. lit
// acquires it without an engine in exactly two ways: LockDoltJournalExclusive, taken by
// a file-by-file copy of the Dolt directory (the snapshot copy and the
// on-change mirror's clone) that must exclude engine-lifecycle I/O without
// opening an engine; and RecordPushedHead's chunk-store open
// (pushed_head.go), which takes LOCK the way a write engine does — through
// dolt's own loader, fail-fast on contention, retried for
// coResidentHolderWait — to move one remote-tracking ref and close.
// That standing Store hold is the trap in the natural
// reading of "hold Dolt's LOCK during a walk": taking LOCK (opening a write
// engine, or locking the file directly) while holding the commit lock
// inverts the order against every live write Store. Take it before commit
// or not at all. One deviation is tolerated, not copied: a GC-contention
// retry rotates the store's connection mid-mutation, re-acquiring LOCK
// under the held commit lock. It cannot wedge — each re-open's wait is
// bounded (every LOCK taker but the pushed-head record then waits on the
// commit lock, so none can keep arriving in front of it past
// coResidentHolderWait; a routine taker heartbeats through its own open, but
// its commit-lock wait runs in lit, outside its engine, so its heartbeat
// stops there, and the pushed-head record's handle waits on the commit lock
// outside its engine too) and the retry makes that re-open at most once, only
// while the mutation's whole hold still fits commitLockWaiterBudget, so the
// inverted edge always breaks by the re-open failing the mutation loudly —
// and BOTH bounds are the tolerance's whole justification, because the
// per-open one alone leaves the hold free to outlast every waiter on the
// lock; see Store.reconnect and retryTransientGCContention. (The one write
// engine outside the Store lifecycle, the adopt clone's, runs under the
// exclusive workspace hold and never takes the commit lock.)
//
// ONE ROOT PER ENGINE. An engine reads the root it loaded when it opened and
// never reloads it: Dolt rebases a session's root at transaction start only
// for a read replica (dsess StartTransaction), which lit never configures. A
// write engine holds LOCK, so no other writer moves the root under it; a read
// engine is read-only and never sees the commits of a writer beside it. So
// an engine answers from the moment it opened plus its Store's own writes. A Store changes engines at two places: at
// Store.reconnect, inside a mutation under the commit lock, and at
// withEngineClosed, between commit-lock holds, for a follow of a re-rooted
// remote or the history bound's re-root — each must reopen, since its engine
// would never see the branch it rewrote. Either way the new
// engine loads the root as it then stands, and neither runs inside a read. A
// read call
// therefore answers from one moment however many statements it runs — the
// relation gather's two endpoint queries and its issue lookup, ListIssues'
// scan and the label and lifecycle reads that hydrate it. TestGetRelationsByIDsAnswersFromTheMomentItsStoreOpened pins
// it. Should an engine ever reload its root mid-life, every multi-statement
// read in this package tears at once, and the fix is a read transaction over
// the whole call.
//
// The "one write-capable engine per path" fact has one lock: every engine
// open contends on LOCK itself with a bounded retry. A lock minted beside
// LOCK, under any name, is a partial shadow of it and creates the
// two-representations disagreement.
//
// The sync-push lock sits outside the slots: its holder goes on to take
// everything in them (the mirror cycle holds workspace, LOCK and commit for
// its clone, then opens a full write Store on the clone's own path, then
// takes LOCK and commit again to record the pushed head), but every
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
// A write engine's hold of LOCK reports on itself (lock_hold.go): Dolt tells
// it the instants LOCK is taken and let go, and a routine hold heartbeats
// through LOCK's arrival file while its engine works, which a contender reads
// as progress. The heartbeat is a write with no flock, so it adds no wait
// edge, and a holder waiting on any lock is outside its engine and outside
// any transaction (the commit lock is taken before one begins) and sends
// none, so no wait cycle can keep itself alive on heartbeats.
//
// Holder records sit outside the slots as well. Every acquisition through
// acquireStoreLock publishes one — a uniquely-named file under <storage
// dir>/.links-lock-holders/<lock file name>/, held SHARED for exactly the
// life of the hold it describes — so a contender can name the pid,
// command, and age of what is blocking it instead of guessing — and first
// retires every record there whose holder is gone, so no record outlives
// its holder past the next acquisition. They sit
// under the storage dir and never beside the lock, because one lock
// (Dolt's journal LOCK) lives in a directory lit does not own, and lit's
// records do not go there. They are descriptive only: the kernel remains
// the sole authority on whether a lock is held, and no lock decision reads
// a record's contents, so a wrong record can cost a diagnostic and nothing
// else. No wait edge exists here at all: a reader's liveness probe passes
// maxAttempts 1, and a publisher holds only a private name no reader can
// reach, so neither ever waits on a record's flock. See lock_holder.go.
//
// The config lock sits outside the slots, and outside this package: the
// workspace package mints it beside config.json and holds it exclusively
// across each read-decide-write of that file, because workspace resolution
// runs before any store exists and sits below this package. It is a leaf —
// its holder reads and writes config.json and acquires nothing else — so any
// holder of any slot may take it, and no wait edge leaves it. See
// workspace.withConfigLock.
//
// The backups lock sits outside the slots too: the backup package mints it
// in the storage dir and holds it exclusively across each take's read of the
// store, its write and its prune. It is a leaf as well — the read is a read
// Store's export or an export already in hand, neither of which takes a lock —
// so a restore holding Dolt's LOCK may take it, and no wait edge leaves it.
// See backup.Take.
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
