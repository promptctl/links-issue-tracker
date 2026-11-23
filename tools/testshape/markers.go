package main

// The functions whose reach marks a test as touching something outside its
// process — the one home of the lists. [LAW:one-source-of-truth] The reach
// analysis and the layer rule never name a function; they read these tables.
//
// A function marker is keyed by its qualified name: an import path, a dot, and
// the function. A key that starts with "./" is a path inside the module being
// measured, so the table names this repository's helpers without spelling its
// module path. Reaching a marker records its touch and stops the walk there:
// what lies behind a marker is that touch, so its body adds nothing.
var funcMarkers = map[string]marker{
	// CLI run: the command dispatched in-process, the way a user runs it.
	"./internal/cli.Run": {cli, "the whole lit command, dispatched in this process"},
	// cmd/lit's own runner builds and execs the lit binary.
	"./cmd/lit.runLit": {cli, "execs the built lit binary"},

	// Store on disk: anything that opens a Dolt store in a directory.
	"./internal/app.Open":                {store, "opens the workspace's store on disk"},
	"./internal/app.OpenLocationForRead": {store, "opens a store on disk to read"},
	"./internal/engine.Open":             {store, "opens a Dolt store on disk"},
	"./internal/store.Open":              {store, "opens a Dolt store on disk"},
	"./internal/store.OpenRoutine":       {store, "opens a Dolt store on disk"},
	"./internal/store.OpenSync":          {store, "opens a Dolt store on disk for sync"},
	"./internal/store.OpenMaintenance":   {store, "opens a Dolt store on disk for maintenance"},
	"./internal/store.OpenForRead":       {store, "opens a Dolt store on disk to read"},
	"database/sql.Open":                  {store, "opens a SQL database; in this module that is Dolt on disk"},
	"database/sql.OpenDB":                {store, "opens a SQL database; in this module that is Dolt on disk"},

	// git: Dolt's git command runner, which internal/environ's Env.Git wraps.
	"github.com/dolthub/dolt/go/libraries/utils/gitcmd.Command": {git, "starts a git process"},

	// Filesystem.
	"os.Chdir":                   {fs, "changes the process's working directory"},
	"os.Chmod":                   {fs, "changes a file's mode"},
	"os.Create":                  {fs, "creates a file"},
	"os.CreateTemp":              {fs, "creates a file"},
	"os.DirFS":                   {fs, "reads a directory tree"},
	"os.Getwd":                   {fs, "reads the working directory"},
	"os.Link":                    {fs, "creates a hard link"},
	"os.Lstat":                   {fs, "stats a path"},
	"os.Mkdir":                   {fs, "creates a directory"},
	"os.MkdirAll":                {fs, "creates directories"},
	"os.MkdirTemp":               {fs, "creates a directory"},
	"os.Open":                    {fs, "opens a file"},
	"os.OpenFile":                {fs, "opens a file"},
	"os.OpenRoot":                {fs, "opens a directory"},
	"os.ReadDir":                 {fs, "lists a directory"},
	"os.ReadFile":                {fs, "reads a file"},
	"os.Readlink":                {fs, "reads a symlink"},
	"os.Remove":                  {fs, "removes a path"},
	"os.RemoveAll":               {fs, "removes a tree"},
	"os.Rename":                  {fs, "renames a path"},
	"os.Stat":                    {fs, "stats a path"},
	"os.Symlink":                 {fs, "creates a symlink"},
	"os.Truncate":                {fs, "truncates a file"},
	"os.WriteFile":               {fs, "writes a file"},
	"path/filepath.Abs":          {fs, "reads the working directory"},
	"path/filepath.EvalSymlinks": {fs, "resolves symlinks on disk"},
	"path/filepath.Glob":         {fs, "lists a directory"},
	"path/filepath.Walk":         {fs, "walks a directory tree"},
	"path/filepath.WalkDir":      {fs, "walks a directory tree"},

	// Real clock, waiting on it.
	"time.After":                {clockWait, "waits on the wall clock"},
	"time.AfterFunc":            {clockWait, "waits on the wall clock"},
	"time.NewTicker":            {clockWait, "waits on the wall clock"},
	"time.NewTimer":             {clockWait, "waits on the wall clock"},
	"time.Sleep":                {clockWait, "waits on the wall clock"},
	"time.Tick":                 {clockWait, "waits on the wall clock"},
	"context.WithDeadline":      {clockWait, "a deadline that runs against the wall clock"},
	"context.WithTimeout":       {clockWait, "a deadline that runs against the wall clock"},
	"context.WithDeadlineCause": {clockWait, "a deadline that runs against the wall clock"},
	"context.WithTimeoutCause":  {clockWait, "a deadline that runs against the wall clock"},

	// Real clock, read as a value.
	"time.Now":   {clockRead, "reads the wall clock"},
	"time.Since": {clockRead, "reads the wall clock"},
	"time.Until": {clockRead, "reads the wall clock"},
}

// methodMarkers are methods, matched by name alone: without type information
// the receiver of `x.TempDir()` is unknown, so a marker here must be a method
// name nothing else in the module declares. Each entry names its receiver.
var methodMarkers = map[string]marker{
	"TempDir": {fs, "(testing.TB).TempDir creates a directory on disk"},
	"Chdir":   {fs, "(testing.TB).Chdir changes the process's working directory"},
	"Git":     {git, "(environ.Env).Git starts a git process"},
}

// spawnMarkers start a child process; nameArg is the index of the argument
// that names the program. What that argument is decides the touch (see
// spawnTouch): git, the test binary itself (a CLI run), or another program.
// A function of the module that hands one of its own parameters to a spawn
// marker's nameArg is a spawner too, found by the analysis, not listed here.
var spawnMarkers = map[string]spawnMarker{
	"os/exec.Command":        {0, "starts a child process"},
	"os/exec.CommandContext": {1, "starts a child process"},
}

// spawnNames maps a program named by a string literal to its touch. A program
// not listed is a process touch.
var spawnNames = map[string]touch{
	"git": git,
}

type marker struct {
	touch  touch
	reason string
}

type spawnMarker struct {
	nameArg int
	reason  string
}
