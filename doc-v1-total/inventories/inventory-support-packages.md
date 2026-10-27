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
- `Read(path)` (`syncfile.go`): reads the file and decodes it with `Decode`; a read
  error is `"read sync file: %w"`.
- `Decode(payload)` (`syncfile.go`): unmarshals into `model.Export` (inheriting the
  v1→v2 history conversion in `Export.UnmarshalJSON`), returns the export plus the
  payload's SHA-256. Error: `"parse sync file: %w"`.
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

## `internal/backup` (`backup.go`, 203 lines)

JSON export snapshots under `<storageDir>/backups/`, named by content digest.

- `Snapshot{Path, Name, Created, Size}` with JSON tags `path/name/created/size`
  (`backup.go`).
- `Budget = 20`: the `lit backup create --keep` default and the restore's prune.
- `Take(ctx, storageDir, keep, read) (*Taken, error)` (`backup.go`): the one writer of the set. `keep <= 0` → `"keep must be > 0"` before anything is read; holds `<storageDir>/.links-backups.lock` exclusively (filelock, 3000 × 10ms; contention → `backup.ErrBusy`, reason `workspace_busy`) while it calls `read`, writes the export and prunes, so a read made before the lock cannot land after a newer one and no prune removes a take it did not list; `nil` when nothing was written, and a prune failure returns the `Taken` with `"prune backups after writing %s: %w"`. `Taken.Waited` is the wait for the lock and `Taken.Pruned` the count the prune removed.
- `create` (`backup.go`): ensures `<storageDir>/backups`
  (`0o755`); filename is `syncfile.ContentHash(export) + ".json"`: a snapshot is named by its content, `exported_at` left out, so a take of content the set already holds replaces the snapshot of that name, which becomes the newest, and the set holds each content once; `Taken.Refreshed` says whether the name already existed;
  writes via `syncfile.WriteAtomic` (same indented-JSON format and atomicity as sync
  files); stats the result and returns `Created` = file mtime (UTC), `Size` in bytes.
- `List(storageDir)` (`backup.go`): missing backups dir → empty list, no error;
  skips directories and non-`.json` names; `Created` from each entry's mtime (UTC);
  sorted newest-first.
- `prune` (`backup.go`): returns the count it removed; removes every snapshot after the first `keep` in newest-first order; stops with
  `"remove backup %s: %w"` on the first removal failure.
- `Latest(storageDir)` (`backup.go`): newest snapshot or `nil` when none exist;
  never an error for an empty/missing dir.
- Selection is mtime-based throughout: `List`/`Prune`/`Latest` order by file
  modification time; a name carries no time. A snapshot an older lit named by its
  time (`20060102-150405.000000000.json`) is listed, restorable and pruned like any.
