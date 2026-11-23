package main

// The markers and spawn programs: the one home of what testshape counts as a
// touch and of every name it resolves a spawned program by.
// [LAW:one-source-of-truth] Each name is a declaration of one package, named
// within it as go/types prints it: "Open", "(*Client).Do", a variable "Args",
// a struct field "Cmd.Path". A module marker's package path leaves the module
// path off. A run fails on a name its package does not declare, each package
// loaded by its own path, and TestEveryMarkerNamesADeclaredFunction holds
// this module to the same check.

// moduleMarkers are packages of the module being measured, named with the
// module path left off: "internal/cli" is <module>/internal/cli. The module
// path is the loaded program's, so these names follow the module.
var moduleMarkers = []markerGroup{
	// A CLI run in process. A spawned CLI run is decided by what is spawned
	// (see the package doc), and every main package's main is a CLI run too.
	{"internal/cli", touchCLI, []string{"Run"}},

	// A store on disk: the app and engine openers, which construct the store
	// a test then reaches only through storage.Store, and the Dolt store's own
	// openers. Dolt's loader and database/sql's openers, below, catch any open
	// that goes around them.
	{"internal/app", touchStore, []string{"Open", "OpenLocationForRead"}},
	{"internal/engine", touchStore, []string{"Open"}},
	{"internal/store", touchStore, []string{"Open", "OpenRoutine", "OpenForRead", "OpenSync", "OpenMaintenance"}},

	// git: every git lit itself starts is started by Env.Git.
	{"internal/environ", touchGit, []string{"(Env).Git"}},
}

// externalMarkers are packages outside the module: the standard library's
// and the dependencies' functions that leave the process. A call from module
// code to any other function outside the module is not followed, so the
// testing package's own clock reads, log timestamps and net/http's internals
// are never charged.
var externalMarkers = []markerGroup{
	// A store on disk.
	{"database/sql", touchStore, []string{"Open", "OpenDB"}},
	{"github.com/dolthub/dolt/go/libraries/doltcore/doltdb", touchStore, []string{"LoadDoltDBWithParams"}},

	// git: Dolt's git, which Env.Git wraps.
	{"github.com/dolthub/dolt/go/libraries/utils/gitcmd", touchGit, []string{"Command"}},

	// Another process, started inside a dependency the walk does not enter:
	// go/packages runs `go list`, and so does the gc importer go/importer
	// constructs, to find export data.
	{"golang.org/x/tools/go/packages", touchProc, []string{"Load"}},
	{"go/importer", touchProc, []string{"Default", "For", "ForCompiler"}},

	// The filesystem: the os, io/ioutil and path/filepath functions that read
	// or write the disk, and the test harness's own directories.
	{"os", touchFS, []string{
		"Open", "OpenFile", "OpenRoot", "OpenInRoot", "Create", "CreateTemp",
		"ReadFile", "WriteFile", "ReadDir", "DirFS", "CopyFS", "Stat", "Lstat",
		"Mkdir", "MkdirAll", "MkdirTemp", "Remove", "RemoveAll", "Rename", "Link",
		"Symlink", "Readlink", "Chmod", "Chown", "Lchown", "Chtimes", "Truncate",
		"Chdir",
	}},
	{"io/ioutil", touchFS, []string{"ReadFile", "WriteFile", "ReadDir", "TempDir", "TempFile"}},
	{"path/filepath", touchFS, []string{"Walk", "WalkDir", "Glob", "EvalSymlinks"}},
	{"testing", touchFS, []string{
		"(*common).TempDir", "(*common).Chdir",
		// [LAW:types-are-the-program] exception: interface methods, matched
		// on the call through the interface, which every other call through
		// an interface is not. testing.TB is the harness's own interface, and
		// every implementation of these methods is the harness's directory,
		// so the call through TB is the touch wherever the TB came from.
		"(TB).TempDir", "(TB).Chdir",
	}},

	// The real clock.
	{"time", touchClock, []string{"Now", "Since", "Until", "Sleep", "After", "AfterFunc", "Tick", "NewTimer", "NewTicker"}},
	{"context", touchClock, []string{"WithDeadline", "WithDeadlineCause", "WithTimeout", "WithTimeoutCause"}},

	// The network.
	{"net", touchNetwork, []string{
		"Listen", "ListenPacket", "ListenTCP", "ListenUDP", "ListenUnix",
		"(*ListenConfig).Listen", "(*ListenConfig).ListenPacket",
		"Dial", "DialTimeout", "DialTCP", "DialUDP", "DialUnix",
		"(*Dialer).Dial", "(*Dialer).DialContext", "LookupHost", "LookupIP",
	}},
	{"net/http", touchNetwork, []string{
		"Get", "Head", "Post", "PostForm",
		"(*Client).Do", "(*Client).Get", "(*Client).Head", "(*Client).Post", "(*Client).PostForm",
		"ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS",
		"(*Server).ListenAndServe", "(*Server).ListenAndServeTLS", "(*Server).Serve", "(*Server).ServeTLS",
	}},
	{"net/http/httptest", touchNetwork, []string{"NewServer", "NewTLSServer", "NewUnstartedServer"}},
}

// spawners are the functions that start a process, each with the index of
// the argument that names the program. A call's program is resolved by
// value: git's name, or a path ending in /git, is git; this test binary
// itself (selfExecutable, selfArgs[0]) is decided by the package's TestMain;
// any other constant is another process, and a program no constant names is
// unresolved, another process too. Reached as a value instead of called, a
// spawner's program is unresolved.
var spawners = []spawner{
	{decl{"os/exec", "Command"}, 0},
	{decl{"os/exec", "CommandContext"}, 1},
	{decl{"os", "StartProcess"}, 0},
	{decl{"syscall", "ForkExec"}, 0},
	{decl{"syscall", "Exec"}, 0},
}

// The program names and declarations a spawn's program is resolved through.
const gitName = "git"

var (
	selfExecutable = decl{"os", "Executable"}    // this test binary's path
	selfArgs       = decl{"os", "Args"}          // its element 0 is this test binary
	lookPath       = decl{"os/exec", "LookPath"} // a program's name to its path
	// An exec.Cmd built as a value spawns the program stored in its Path.
	cmdPath = decl{"os/exec", "Cmd.Path"}
)

// mainTouch is what reaching the main function of a module package named
// main is: each of the module's main packages is a CLI.
const mainTouch = touchCLI

// resolvedThrough are the declarations above, which a run checks as it
// checks every marker.
var resolvedThrough = []decl{selfExecutable, selfArgs, lookPath, cmdPath}
