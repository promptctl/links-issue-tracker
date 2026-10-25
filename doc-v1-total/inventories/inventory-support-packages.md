# Behavioral Inventory — support packages: atomicfile, syncfile, engine, backup

Written directly from source by the coordinating session (not a subagent) after the
store-sync inventory was found not to cover these four packages. Method identical to
the other inventories: Go source only, no docs read, every claim cited.

## `internal/atomicfile` (`atomicfile.go`, 124 lines)

Atomic file replacement; imports only the standard library (`.golangci.yml`,
`atomicfile-stdlib-only`), so `internal/statusline` may import it.

- `Replace(name, perm, durability, write)` (`atomicfile.go`): `os.CreateTemp` in
  `filepath.Dir(name)` with pattern `filepath.Base(name) + ".tmp-*"` (it extends
  the target's name and never ends in the target's extension); runs `write` on the temp
  file; `Chmod(perm)` on the handle (the mode is exact, whatever the umask); for
  `Durable`, `Sync()`; `Close()`; `os.Rename` over `name`; for `Durable`, a
  best-effort fsync of the directory whose error is deliberately ignored (Windows
  cannot fsync a directory handle). The directory is not created. On any failure
  the temp file is removed and a removal error is joined into the returned error;
  `write`'s own error is returned wrapped but not reworded, and the package's own steps are named:
  `create temp file: %w`, `chmod: %w`, `fsync: %w`, `close: %w`, `rename: %w`.
- `WriteFile(name, data, perm, durability)` (`atomicfile.go`): `Replace` with a
  `write` that writes `data`.
- `Durability` (`atomicfile.go`): `Replaceable` (no fsync) or `Durable`; any other
  value, the zero value included, panics before the temp file is created.
- Callers and their `(perm, durability)`: statusline files `(0o644, Replaceable)`;
  workspace `config.json` `(0o644, Durable)`; the installed binary
  `(0o755, Durable)`; the cli's storage-dir markers `(0o600, Replaceable)`;
  `syncfile.WriteAtomic` `(0o600, Durable)`; `received-refs.last`
  `(0o600, Replaceable)`; the adopt-pending marker `(0o600, Durable)`.

## `internal/syncfile` (`syncfile.go`, 67 lines)

JSON export files on disk, with content hashing.

- `WriteAtomic(path, export)` (`syncfile.go`): marshals a `model.Export` with
  `json.MarshalIndent(export, "", "  ")` plus a trailing newline (`syncfile.go`);
  creates the parent directory `0o755`; replaces `path` through
  `atomicfile.WriteFile` with mode `0o600`, durably; a failure is
  `write sync file: %w`. Returns the
  lowercase hex SHA-256 of the exact payload bytes (`syncfile.go`).
- `Read(path)` (`syncfile.go`): reads the file, unmarshals into `model.Export`
  (inheriting the v1→v2 history conversion in `Export.UnmarshalJSON`), returns the
  export plus the payload's SHA-256. Errors: `"read sync file: %w"`, `"parse sync file: %w"`.
- `ContentHash(export)` (`syncfile.go`): SHA-256 of the export marshalled as
  `WriteAtomic` writes it but with `ExportedAt` zeroed, so it covers what the
  export holds, not when it was taken. Given only store exports: a decoded epic
  awaits hydration and refuses to marshal.
- Hash format everywhere: lowercase hex SHA-256 of the serialized bytes, so any
  whitespace or key-order change alters the hash.

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
