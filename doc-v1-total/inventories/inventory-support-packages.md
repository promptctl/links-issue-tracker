# Behavioral Inventory — support packages: syncfile, doltcli, engine, backup

Written directly from source by the coordinating session (not a subagent) after the
store-sync inventory was found not to cover these four packages. Method identical to
the other inventories: Go source only, no docs read, every claim cited.

## `internal/syncfile` (`syncfile.go`, 77 lines)

JSON export files on disk, with content hashing.

- `WriteAtomic(path, export)` (`syncfile.go`): marshals a `model.Export` with
  `json.MarshalIndent(export, "", "  ")` plus a trailing newline (`syncfile.go`);
  creates the parent directory `0o755`; writes to a temp file named `.links-sync-*.json`
  in the destination directory; renames over `path` (atomic on the same filesystem);
  the temp file is removed on any failure path via deferred `os.Remove`. Returns the
  lowercase hex SHA-256 of the exact payload bytes (`syncfile.go`).
- `Read(path)` (`syncfile.go`): reads the file, unmarshals into `model.Export`
  (inheriting the v1→v2 history conversion in `Export.UnmarshalJSON`), returns the
  export plus the payload's SHA-256. Errors: `"read sync file: %w"`, `"parse sync file: %w"`.
- `HashFile(path)` (`syncfile.go`): SHA-256 of the file's bytes; a missing file
  returns `("", nil)` — absence is not an error.
- Hash format everywhere: lowercase hex SHA-256 of the serialized bytes, so any
  whitespace or key-order change alters the hash.

## `internal/doltcli` (`doltcli.go`, 99 lines)

Shell-out wrapper for an external `dolt` binary (as opposed to the embedded engine).

- `MinSupportedVersion = "1.81.10"` (`doltcli.go`).
- `Version{Major, Minor, Patch}` with `String()` → `"%d.%d.%d"` and lexicographic
  `LessThan` (`doltcli.go`).
- `ParseVersion` (`doltcli.go`): extracts the first `\d+\.\d+\.\d+` match from
  the (trimmed) input via regexp; no match → `"parse dolt version from %q"`.
- `InstalledVersion(ctx, cwd)` (`doltcli.go`): runs `dolt version` in `cwd`,
  parses the output.
- `RequireMinimumVersion(ctx, cwd, minRequired)` (`doltcli.go`): parses the
  floor, reads the installed version, and refuses with
  `"dolt %s+ is required, found %s"` when installed < floor. Returns the installed
  version on success.
- `Run(ctx, cwd, args...)` (`doltcli.go`): refuses empty args
  (`"dolt args are required"`); otherwise executes `dolt <args...>` in `cwd`.
- `runCommand` (`doltcli.go`): `exec.CommandContext`, `CombinedOutput`; on
  failure the error is `"dolt <args joined by space>: <trimmed combined output>"`,
  falling back to the exec error's text when output is empty; on success returns
  trimmed combined output.

## `internal/engine` (`engine.go`, 89 lines)

The one seam that maps an open mode to a concrete store constructor; the only package
above `internal/store` that names the Dolt engine's construction surface (package doc,
`engine.go`).

- `Mode` is a string type with three values and no meaningful zero value
  (`engine.go`):

  | Mode | Constructor | Contract |
  |---|---|---|
  | `read-write` | `store.Open` | bootstraps an absent database; exclusive workspace lock |
  | `read-only` | `store.OpenForRead` | requires an initialized database; shared lock; never creates a workspace |
  | `sync` | `store.OpenSync` | handle held across network operations; locking admits the mirror beside it |

- The mode→constructor mapping is the package-level `openers` table (`engine.go`).
- `Open(ctx, mode, doltRootDir, workspaceID)` (`engine.go`): unknown mode —
  including the zero value — fails with `"invalid storage engine mode %q"` rather than
  defaulting; on constructor error returns `nil, err` explicitly so a typed-nil
  `*store.Store` can never travel inside a non-nil `storage.Store` interface. The
  return type is `storage.Store`; callers reach optional capabilities via
  `storage.<Cap>.Of` on the returned value.

## `internal/backup` (`backup.go`, 102 lines)

Timestamped JSON export snapshots under `<storageDir>/backups/`.

- `Snapshot{Path, Name, Created, Size}` with JSON tags `path/name/created/size`
  (`backup.go`).
- `Create(storageDir, export)` (`backup.go`): ensures `<storageDir>/backups`
  (`0o755`); filename is `time.Now().UTC().Format("20060102-150405.000000000") + ".json"`;
  writes via `syncfile.WriteAtomic` (same indented-JSON format and atomicity as sync
  files); stats the result and returns `Created` = file mtime (UTC), `Size` in bytes.
- `List(storageDir)` (`backup.go`): missing backups dir → empty list, no error;
  skips directories and non-`.json` names; `Created` from each entry's mtime (UTC);
  sorted newest-first.
- `Prune(storageDir, keep)` (`backup.go`): `keep <= 0` → `"keep must be > 0"`;
  removes every snapshot after the first `keep` in newest-first order; stops with
  `"remove backup %s: %w"` on the first removal failure.
- `Latest(storageDir)` (`backup.go`): newest snapshot or `nil` when none exist;
  never an error for an empty/missing dir.
- Selection is mtime-based throughout: `List`/`Prune`/`Latest` order by file
  modification time, not by parsing the timestamp in the filename.
