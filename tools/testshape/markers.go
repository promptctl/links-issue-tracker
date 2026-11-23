package main

// The markers and spawn programs: the one home of what testshape counts as a
// touch. [LAW:one-source-of-truth] Every entry is a function's full name as
// go/types prints it (types.Func.FullName), so it names one declared function,
// method or interface method, never a spelling at a call site.
// TestEveryModuleMarkerNamesADeclaredFunction loads this module and fails on
// an entry that names nothing.

// moduleMarkers are functions of the module being measured, named with the
// module path left off: "internal/cli.Run" is <module>/internal/cli.Run.
// The module path is the loaded program's, so these names follow the module.
var moduleMarkers = []marker{
	// A CLI run in process. A spawned CLI run is decided by what is spawned
	// (see the package doc), and every main package's main is a CLI run too.
	{"internal/cli.Run", touchCLI},

	// A store on disk: the app and engine openers, which construct the store
	// a test then reaches only through storage.Store, and the Dolt store's own
	// openers. database/sql's openers, below, catch any open that goes around
	// them.
	{"internal/app.Open", touchStore},
	{"internal/app.OpenLocationForRead", touchStore},
	{"internal/engine.Open", touchStore},
	{"internal/store.Open", touchStore},
	{"internal/store.OpenRoutine", touchStore},
	{"internal/store.OpenForRead", touchStore},
	{"internal/store.OpenSync", touchStore},
	{"internal/store.OpenMaintenance", touchStore},

	// git: every git lit itself starts is started by Env.Git.
	{"(internal/environ.Env).Git", touchGit},
}

// externalMarkers are functions outside the module: the standard library's
// and the dependencies' functions that leave the process. A call from module
// code to any other function outside the module is not followed, so the
// testing package's own clock reads, log timestamps and net/http's internals
// are never charged.
var externalMarkers = []marker{
	// A store on disk.
	{"database/sql.Open", touchStore},
	{"database/sql.OpenDB", touchStore},

	// git: Dolt's git, which Env.Git wraps.
	{"github.com/dolthub/dolt/go/libraries/utils/gitcmd.Command", touchGit},

	// The filesystem: the os, io/ioutil and path/filepath functions that read
	// or write the disk, and the test harness's own directories.
	{"os.Open", touchFS},
	{"os.OpenFile", touchFS},
	{"os.OpenRoot", touchFS},
	{"os.OpenInRoot", touchFS},
	{"os.Create", touchFS},
	{"os.CreateTemp", touchFS},
	{"os.ReadFile", touchFS},
	{"os.WriteFile", touchFS},
	{"os.ReadDir", touchFS},
	{"os.DirFS", touchFS},
	{"os.CopyFS", touchFS},
	{"os.Stat", touchFS},
	{"os.Lstat", touchFS},
	{"os.Mkdir", touchFS},
	{"os.MkdirAll", touchFS},
	{"os.MkdirTemp", touchFS},
	{"os.Remove", touchFS},
	{"os.RemoveAll", touchFS},
	{"os.Rename", touchFS},
	{"os.Link", touchFS},
	{"os.Symlink", touchFS},
	{"os.Readlink", touchFS},
	{"os.Chmod", touchFS},
	{"os.Chown", touchFS},
	{"os.Lchown", touchFS},
	{"os.Chtimes", touchFS},
	{"os.Truncate", touchFS},
	{"os.Chdir", touchFS},
	{"io/ioutil.ReadFile", touchFS},
	{"io/ioutil.WriteFile", touchFS},
	{"io/ioutil.ReadDir", touchFS},
	{"io/ioutil.TempDir", touchFS},
	{"io/ioutil.TempFile", touchFS},
	{"path/filepath.Walk", touchFS},
	{"path/filepath.WalkDir", touchFS},
	{"path/filepath.Glob", touchFS},
	{"path/filepath.EvalSymlinks", touchFS},
	{"(*testing.common).TempDir", touchFS},
	{"(*testing.common).Chdir", touchFS},
	// testing.TB is the harness's own interface: every implementation of
	// these methods is the harness's directory, so calling one through TB is
	// the touch, unlike a call through an interface the module declares.
	{"(testing.TB).TempDir", touchFS},
	{"(testing.TB).Chdir", touchFS},

	// The real clock.
	{"time.Now", touchClock},
	{"time.Since", touchClock},
	{"time.Until", touchClock},
	{"time.Sleep", touchClock},
	{"time.After", touchClock},
	{"time.AfterFunc", touchClock},
	{"time.Tick", touchClock},
	{"time.NewTimer", touchClock},
	{"time.NewTicker", touchClock},
	{"context.WithDeadline", touchClock},
	{"context.WithDeadlineCause", touchClock},
	{"context.WithTimeout", touchClock},
	{"context.WithTimeoutCause", touchClock},

	// The network.
	{"net.Listen", touchNetwork},
	{"net.ListenPacket", touchNetwork},
	{"net.ListenTCP", touchNetwork},
	{"net.ListenUDP", touchNetwork},
	{"net.ListenUnix", touchNetwork},
	{"(*net.ListenConfig).Listen", touchNetwork},
	{"(*net.ListenConfig).ListenPacket", touchNetwork},
	{"net.Dial", touchNetwork},
	{"net.DialTimeout", touchNetwork},
	{"net.DialTCP", touchNetwork},
	{"net.DialUDP", touchNetwork},
	{"net.DialUnix", touchNetwork},
	{"(*net.Dialer).Dial", touchNetwork},
	{"(*net.Dialer).DialContext", touchNetwork},
	{"net.LookupHost", touchNetwork},
	{"net.LookupIP", touchNetwork},
	{"net/http.Get", touchNetwork},
	{"net/http.Head", touchNetwork},
	{"net/http.Post", touchNetwork},
	{"net/http.PostForm", touchNetwork},
	{"(*net/http.Client).Do", touchNetwork},
	{"(*net/http.Client).Get", touchNetwork},
	{"(*net/http.Client).Head", touchNetwork},
	{"(*net/http.Client).Post", touchNetwork},
	{"(*net/http.Client).PostForm", touchNetwork},
	{"net/http.ListenAndServe", touchNetwork},
	{"net/http.ListenAndServeTLS", touchNetwork},
	{"net/http.Serve", touchNetwork},
	{"net/http.ServeTLS", touchNetwork},
	{"(*net/http.Server).ListenAndServe", touchNetwork},
	{"(*net/http.Server).ListenAndServeTLS", touchNetwork},
	{"(*net/http.Server).Serve", touchNetwork},
	{"(*net/http.Server).ServeTLS", touchNetwork},
	{"net/http/httptest.NewServer", touchNetwork},
	{"net/http/httptest.NewTLSServer", touchNetwork},
	{"net/http/httptest.NewUnstartedServer", touchNetwork},
}

// spawners are the functions that start a process, each with the index of
// the argument that names the program. A call's program is resolved by
// value: "git", or a path ending in /git, is git; this test binary itself
// (os.Executable, os.Args[0]) is decided by the package's TestMain; anything
// else is another process. Reached as a value instead of called, a spawner's
// program is unknown, and it is another process.
var spawners = []spawner{
	{"os/exec.Command", 0},
	{"os/exec.CommandContext", 1},
	{"os.StartProcess", 0},
	{"syscall.ForkExec", 0},
	{"syscall.Exec", 0},
}

// The values a spawn's program is resolved through: the test binary's own
// path, and the lookup that turns a program's name into its path.
const (
	selfExecutable = "os.Executable"
	selfArgs       = "os.Args"
	lookPath       = "os/exec.LookPath"
	// An exec.Cmd built as a value spawns the program stored in its Path.
	cmdType      = "os/exec.Cmd"
	cmdPathField = "Path"
)

// noReturn are the calls after which a function's block goes no further:
// TestMain's branch that calls one of them before m.Run is a branch a child
// of the test binary takes.
var noReturn = []string{
	"os.Exit",
	"syscall.Exit",
	"log.Fatal",
	"log.Fatalf",
	"log.Fatalln",
	"runtime.Goexit",
}

// mRun is the call that runs a test binary's tests.
const mRun = "(*testing.M).Run"
