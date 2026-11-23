package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// WorkerLauncher starts the detached workers a run spawns: the mirror that
// pushes a write, and the receive that fetches. It is a port the run is
// handed (Invocation.Workers), so which program a worker runs as is decided
// where the run is composed: cmd/lit hands LaunchWorkersAs its own program,
// and a test that must not start a real worker hands one that refuses. What
// to start, and when, stays spawnDetachedWorker's; the launcher only starts
// it. Its one method is unexported, so every launcher is this package's.
// [LAW:effects-at-boundaries]
type WorkerLauncher interface {
	start(w detachedWorker) error
}

// detachedWorker is one detached worker to start: everything
// spawnDetachedWorker decided about it, as values.
type detachedWorker struct {
	// dir is the directory it runs in: its store's, which lives exactly as
	// long as the store.
	dir string
	// args is its command line after the program: the hidden subcommand and
	// its flags.
	args []string
	env  environ.Env
	// handoff is what it reads on its stdin before EOF (workerHandoff).
	handoff []byte
	// holds are the files it inherits from fd 3 on (holdWorkerLock).
	holds []*os.File
	// log receives its stdout and stderr; nil discards both.
	log *os.File
}

// LaunchWorkersAs is the launcher that starts every worker as program, the
// path os.Executable reported, taken in the two values it returns them as. A
// program that could not be resolved fails only the spawns that need it, as
// WorkingDirOf's error fails only what needs the directory.
func LaunchWorkersAs(program string, err error) WorkerLauncher {
	return programLauncher{program: program, err: err}
}

// programLauncher starts a worker as a process of its own, running program,
// detached from the spawner's session.
type programLauncher struct {
	program string
	err     error
}

func (l programLauncher) start(w detachedWorker) error {
	if l.err != nil {
		return fmt.Errorf("resolve lit binary: %w", l.err)
	}
	handoffRead, handoffWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("open the worker's handoff pipe: %w", err)
	}
	// Each end is also closed here, so no path leaks one; the read end is
	// closed early, right after Start, and closing it again is a no-op.
	defer func() { _ = handoffRead.Close(); _ = handoffWrite.Close() }()
	cmd := w.env.Command(w.dir, l.program, w.args...)
	cmd.Stdin = handoffRead
	cmd.ExtraFiles = w.holds
	if w.log != nil {
		cmd.Stdout, cmd.Stderr = w.log, w.log
	}
	cmd.SysProcAttr = detachSysProcAttr()
	startErr := cmd.Start()
	// The child holds its own copy of the read end from exec on. Closing ours
	// is what makes a write to a worker that already died fail instead of
	// waiting on a pipe nobody reads.
	_ = handoffRead.Close()
	if startErr != nil {
		return startErr
	}
	// Written after Start, then closed by the defer, so the worker reads the
	// whole handoff and then EOF. A write the pipe's buffer cannot take waits
	// for the worker, which reads its handoff first.
	//
	// A failed write is a failed spawn: the worker holds the only read end, so
	// the write fails only once the worker has exited, before it could finish
	// reading and so before it recorded anything. Its hold went with it.
	if _, err := handoffWrite.Write(w.handoff); err != nil {
		return fmt.Errorf("write the worker's handoff: %w", err)
	}
	return nil
}

// spawner is the process a detached worker waits out before it opens its
// engine: the command that spawned it (spawnedBy), or none (noSpawner), for a
// worker run in the foreground of a process that holds no engine on its
// store. --parent-pid carries it as the pid or as "none", and a worker given
// neither is refused, so no worker skips the wait by omission.
// [LAW:types-are-the-program]
type spawner interface {
	// awaitExit blocks until the spawner has exited, returning true, or the
	// timeout elapses with it still alive, or ctx ends, returning false.
	awaitExit(ctx context.Context, getppid func() int, timeout, poll time.Duration) bool
	// flag is the spawner as --parent-pid carries it.
	flag() string
	String() string
}

// spawnedBy is the pid of the command that spawned the worker.
type spawnedBy int

// noSpawner is a worker no live engine of its store's precedes.
type noSpawner struct{}

// noSpawnerFlag is noSpawner as --parent-pid carries it.
const noSpawnerFlag = "none"

// parseSpawner is the spawner --parent-pid names: a positive pid, or "none".
// [LAW:parse-dont-validate]
func parseSpawner(flag string) (spawner, error) {
	if flag == noSpawnerFlag {
		return noSpawner{}, nil
	}
	pid, err := strconv.Atoi(flag)
	if err != nil || pid <= 0 {
		return nil, fmt.Errorf("--parent-pid must be the spawning command's pid or %q, got %q", noSpawnerFlag, flag)
	}
	return spawnedBy(pid), nil
}

// awaitExit watches the worker's own parent pid (getppid): the detached worker
// is a direct child of the spawning command, so when that command exits the
// worker is reparented (to init or a subreaper) and getppid stops equalling
// the spawner's pid. This is robust where a kill(pid,0) probe is not — a
// zombie parent still answers kill(pid,0) until it is reaped (delaying the
// worker past the actual exit), and a reused pid could be mistaken for the
// original; reparenting happens at exit, before reaping, and getppid reports
// the real current parent. [LAW:no-ambient-temporal-coupling] The ordering
// owner is the getppid check, not the sleep, which is only the poll interval;
// the boolean distinguishes "parent exited" from "parent outlived the wait" so
// the caller aborts rather than proceeding blindly on a timeout. getppid is a
// parameter so the wait is testable without a real process tree. A done
// context also ends the wait (false): a worker in the SIGTERM grace window
// must spend it releasing state, not sleeping toward a deadline; the caller
// tells teardown from timeout by ctx.Err().
func (p spawnedBy) awaitExit(ctx context.Context, getppid func() int, timeout, poll time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for getppid() == int(p) {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
	return true
}

func (p spawnedBy) flag() string   { return strconv.Itoa(int(p)) }
func (p spawnedBy) String() string { return "pid " + p.flag() }

// awaitExit has nothing to wait for.
func (noSpawner) awaitExit(context.Context, func() int, time.Duration, time.Duration) bool {
	return true
}

func (noSpawner) flag() string   { return noSpawnerFlag }
func (noSpawner) String() string { return "no spawner" }
