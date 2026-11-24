package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// WorkerLauncher starts the detached workers a run spawns: the mirror that
// pushes a write, and the receive that fetches. It is a port the run is
// handed (Invocation.Workers), so which program a worker runs as is decided
// where the run is composed: cmd/lit hands LaunchWorkersAs its own program,
// and a run that must start no worker hands RefuseWorkers. What to start, and
// when, stays spawnDetachedWorker's; the launcher only starts it. Its one
// method is unexported, so every launcher is this package's.
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
	// log receives its stdout and stderr: the worker's log file, or the null
	// device when none could be opened. A file, never another io.Writer: os/exec
	// copies any other writer through a pipe that only Wait drains, and a
	// detached worker's spawner never waits, so the worker would write into a
	// pipe nobody reads once its spawner exits. [LAW:types-are-the-program]
	log *os.File
}

// LaunchWorkersAs is the launcher that starts every worker as program: lit's
// own executable, which cmd/lit resolved at its boundary.
func LaunchWorkersAs(program string) WorkerLauncher {
	return programLauncher{program: program}
}

// programLauncher starts a worker as a process of its own, running program,
// detached from the spawner's session.
type programLauncher struct{ program string }

func (l programLauncher) start(w detachedWorker) error {
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
	cmd.Stdout, cmd.Stderr = w.log, w.log
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

// RefuseWorkers is the launcher of a run that must start no worker: every
// start fails the spawn with reason, naming the worker it would have started,
// and is handed to refused, so whoever composed the run can fail on it — a
// test fails itself. [LAW:no-silent-failure]
func RefuseWorkers(reason string, refused func(error)) WorkerLauncher {
	return refusingLauncher{reason: reason, refused: refused}
}

type refusingLauncher struct {
	reason  string
	refused func(error)
}

func (r refusingLauncher) start(w detachedWorker) error {
	err := fmt.Errorf("lit %s was not started: %s", strings.Join(w.args, " "), r.reason)
	r.refused(err)
	return err
}

// spawner is the process a detached worker waits out before it opens its
// engine: the command that spawned it, by pid. --parent-pid carries it, and
// only parseSpawner and spawningProcess build one, so every worker has a
// spawner to wait for and none skips the wait by omission or by a pid of
// zero. [LAW:types-are-the-program]
type spawner struct{ pid int }

// spawningProcess is this process as the spawner of the worker it starts.
func spawningProcess() spawner { return spawner{pid: os.Getpid()} }

// parseSpawner is the spawner --parent-pid names: a positive pid.
// [LAW:parse-dont-validate]
func parseSpawner(flag string) (spawner, error) {
	pid, err := strconv.Atoi(flag)
	if err != nil || pid <= 0 {
		return spawner{}, fmt.Errorf("--parent-pid must be the spawning command's pid, got %q", flag)
	}
	return spawner{pid: pid}, nil
}

// awaitExit blocks until the spawner has exited, returning true, or the
// timeout elapses with it still alive, or ctx ends, returning false.
//
// It watches the worker's own parent pid (getppid): the detached worker is a
// direct child of the spawning command, so when that command exits the
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
func (p spawner) awaitExit(ctx context.Context, getppid func() int, timeout, poll time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for getppid() == p.pid {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
	return true
}

// flag is the spawner as --parent-pid carries it.
func (p spawner) flag() string   { return strconv.Itoa(p.pid) }
func (p spawner) String() string { return "pid " + p.flag() }
