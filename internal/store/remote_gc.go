package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// collectRemote runs the remote's GC from this checkout once a bound's
// re-root at head has been pushed to remote's branch. A force push only adds
// table files, and the remote never runs a GC of its own, so without this the
// remote keeps every write it ever took however short its history is.
//
// It runs under the commit lock, as every fetch and push does: the remote is
// opened through the git mirror they all read and write. What keeps a peer's
// write to the remote safe is not the lock but the remote's own manifest,
// which takes each of the GC's writes only over the view it read
// (collectRemoteView). The scratch store goes however the GC ended.
func (s *Store) collectRemote(ctx context.Context, remote, branch, head string) (gc storage.RemoteGC, err error) {
	scratch, err := os.MkdirTemp("", "lit-remote-gc-")
	if err != nil {
		return gc, fmt.Errorf("create the remote GC's scratch store: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		remoteDB, err := s.openRemoteDB(ctx, remote)
		if err != nil {
			return err
		}
		var gcErr error
		gc, gcErr = collectRemoteView(ctx, remoteDB, scratch, branch, head)
		return errors.Join(gcErr, remoteDB.Close())
	})
	return gc, err
}

// collectRemoteView is the remote's GC over the view remoteDB read when it
// opened. Every check reads that view, and every write lands only while the
// remote still holds it, so a peer's write anywhere in between ends the GC
// with the remote whole:
//  1. unless the view's branch is at head the GC ends RemoteGCMoved having
//     written nothing, and a dataset other than a branch is an error before
//     any write (datasetsAt);
//  2. it builds, in scratch, the table files head reaches named as branch
//     alone (buildCollectedSet), from the remote itself, and uploads them;
//  3. it points the remote's root at the scratch store's, which names branch
//     and nothing else, while the remote's root is still the view's
//     (pointRootAt): one write drops every other branch, or the GC ends
//     RemoteGCMoved with every branch kept;
//  4. it points the remote's manifest at exactly the collected table files
//     (replaceTableFiles), while its lock is still step 3's.
func collectRemoteView(ctx context.Context, remoteDB *doltdb.DoltDB, scratch, branch, head string) (gc storage.RemoteGC, err error) {
	db := doltdb.ExposeDatabaseFromDoltDB(remoteDB)
	// [LAW:parse-dont-validate] the one shape the GC can replace: a git
	// remote's store is a single NomsBlockStore over its blobstore.
	remote, ok := datas.ChunkStoreFromDatabase(db).(*nbs.NomsBlockStore)
	if !ok {
		return gc, fmt.Errorf("the remote's store is a %T; the remote's GC replaces the table files of a git remote's store", datas.ChunkStoreFromDatabase(db))
	}
	viewRoot, err := remote.Root(ctx)
	if err != nil {
		return gc, fmt.Errorf("read the remote's root: %w", err)
	}
	if gc.BytesBefore, err = storeBytesOf(ctx, remote); err != nil {
		return gc, err
	}
	keepAt, others, err := datasetsAt(ctx, db, viewRoot, ref.NewBranchRef(branch))
	if err != nil {
		return gc, err
	}
	// [LAW:no-ambient-temporal-coupling] the view owns "a peer wrote first":
	// every step below reads it or lands only over it.
	if keepAt.String() != head {
		gc.Outcome = storage.RemoteGCMoved
		return gc, nil
	}

	collected, err := buildCollectedSet(ctx, scratch, remoteDB, branch, head)
	if err != nil {
		return gc, err
	}
	defer func() { err = errors.Join(err, collected.Close()) }()
	collectedCS := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(collected))
	collectedFiles, ok := collectedCS.(chunks.TableFileStore)
	if !ok {
		return gc, fmt.Errorf("the remote GC's scratch store is a %T, which lists no table files", collectedCS)
	}
	if gc.CollectedBytes, err = storeBytesOf(ctx, collectedFiles); err != nil {
		return gc, err
	}
	sources, err := collectedFiles.Sources(ctx)
	if err != nil {
		return gc, fmt.Errorf("list the collected table files: %w", err)
	}
	files, err := uploadTableFiles(ctx, sources, remote)
	if err != nil {
		return gc, err
	}

	pointed, err := pointRootAt(ctx, collectedCS, sources.Root, remote, viewRoot)
	if err != nil {
		return gc, err
	}
	if !pointed {
		gc.Outcome = storage.RemoteGCMoved
		return gc, nil
	}
	gc.DroppedBranches = others
	gc.Outcome, err = replaceTableFiles(ctx, remote, files)
	return gc, err
}

// datasetsAt reads every dataset root names: the commit keep names, and the
// other branches, none of which the collected files hold.
// Anything else is an error, because the root the GC writes names keep
// alone: the GC drops nothing until it can name everything it drops.
func datasetsAt(ctx context.Context, db datas.Database, root hash.Hash, keep ref.DoltRef) (keepAt hash.Hash, others []string, err error) {
	datasets, err := db.DatasetsByRootHash(ctx, root)
	if err != nil {
		return keepAt, nil, fmt.Errorf("read the remote's datasets: %w", err)
	}
	err = datasets.IterAll(ctx, func(id string, addr hash.Hash) error {
		switch {
		case id == keep.String():
			keepAt = addr
		case isBranch(id):
			others = append(others, id)
		default:
			return fmt.Errorf("the remote holds %s, which is not a branch the remote's GC can drop", id)
		}
		return nil
	})
	return keepAt, others, err
}

// isBranch reports whether a dataset id names a branch. ref.Parse reads an
// id that is not a ref as a branch, so a working set or any other dataset
// outside refs/ has to be told apart first.
func isBranch(id string) bool {
	if !ref.IsRef(id) {
		return false
	}
	r, err := ref.Parse(id)
	return err == nil && r.GetType() == ref.BranchRefType
}

// buildCollectedSet opens a fresh store in dir holding the chunks reachable
// from head in source and a root that names head as branch and nothing else:
// the root the remote holds once its other branches are gone, and table files
// holding everything that root reaches. The store has no chunk journal, so
// every chunk it holds is in a table file the remote can take.
func buildCollectedSet(ctx context.Context, dir string, source *doltdb.DoltDB, branch, head string) (*doltdb.DoltDB, error) {
	nomsDir := filepath.Join(dir, "noms")
	tempDir := filepath.Join(dir, "tmp")
	for _, d := range []string{nomsDir, tempDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create the remote GC's scratch store: %w", err)
		}
	}
	collected, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, "file://"+filepath.ToSlash(nomsDir), filesys.LocalFS, map[string]any{dbfactory.DisableSingletonCacheParam: struct{}{}})
	if err != nil {
		return nil, fmt.Errorf("open the remote GC's scratch store: %w", err)
	}
	addr := hash.Parse(head)
	err = collected.PullChunks(ctx, tempDir, source, []hash.Hash{addr}, nil, nil)
	if err == nil {
		err = collected.SetHead(ctx, ref.NewBranchRef(branch), addr)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("copy what %s reaches into the remote GC's scratch store: %w", head, err), collected.Close())
	}
	return collected, nil
}

// uploadTableFiles writes every table file of sources to the remote, as
// Dolt's clone writes a table file to its sink, and returns them as the
// table set the remote's manifest takes. Writing a table file does not touch
// the remote's manifest. A chunk journal or an appendix is not a shape the
// collected store writes, so either one is an error rather than a file left
// behind.
func uploadTableFiles(ctx context.Context, sources chunks.TableFileSources, remote *nbs.NomsBlockStore) (map[hash.Hash]uint32, error) {
	if len(sources.AppendixTableFiles) > 0 {
		return nil, fmt.Errorf("the collected store lists %d appendix table files, which the remote's GC does not write", len(sources.AppendixTableFiles))
	}
	files := make(map[hash.Hash]uint32, len(sources.TableFiles))
	for _, file := range sources.TableFiles {
		name, ok := hash.MaybeParse(file.FileID())
		if !ok || file.FileID() == chunks.JournalFileID {
			return nil, fmt.Errorf("the collected store lists %q, which is not a table file the remote can take", file.FileID())
		}
		open := func() (io.ReadCloser, uint64, error) { return file.Open(ctx) }
		if err := remote.WriteTableFile(ctx, file.FileID()+file.LocationSuffix(), file.SplitOffset(), file.NumChunks(), nil, open); err != nil {
			return nil, fmt.Errorf("write table file %s to the remote: %w", file.FileID(), err)
		}
		files[name] = uint32(file.NumChunks())
	}
	return files, nil
}

// pointRootAt makes root, read from collected, the remote's root while the
// remote's root is still last, and reports whether it did. It is one
// compare-and-swap (nbs Commit, which never retries over another root), and
// the remote checks root's references are chunks it holds before it lands.
func pointRootAt(ctx context.Context, collected chunks.ChunkStore, root hash.Hash, remote *nbs.NomsBlockStore, last hash.Hash) (bool, error) {
	chunk, err := collected.Get(ctx, root)
	if err != nil {
		return false, fmt.Errorf("read the collected root %s: %w", root, err)
	}
	getAddrs := func(c chunks.Chunk) chunks.GetAddrsCb {
		return func(_ context.Context, addrs hash.HashSet, _ chunks.PendingRefExists) error {
			return types.AddrsFromNomsValue(c, types.Format_Default, addrs)
		}
	}
	if err := remote.Put(ctx, chunk, getAddrs); err != nil {
		return false, fmt.Errorf("write the collected root %s to the remote: %w", root, err)
	}
	pointed, err := remote.Commit(ctx, root, last)
	if err != nil {
		return false, fmt.Errorf("point the remote's root at %s: %w", root, err)
	}
	return pointed, nil
}

// replaceTableFiles points the remote's manifest at exactly files, under a
// new GC generation (nbs.ReplaceStoreTableFiles, dolt patch 10), while the
// manifest's lock is the one the remote last wrote. A write that landed in
// between leaves the table set whole, with the other branches already
// dropped. The new generation is what keeps a writer whose view predates the
// GC from landing table files that reference a chunk it dropped: that writer
// re-checks them against the new files first.
func replaceTableFiles(ctx context.Context, remote *nbs.NomsBlockStore, files map[hash.Hash]uint32) (storage.RemoteGCOutcome, error) {
	err := nbs.ReplaceStoreTableFiles(ctx, remote, files)
	switch {
	case errors.Is(err, nbs.ErrConcurrentManifestWriteDuringOverwrite):
		return storage.RemoteGCDropped, nil
	case err != nil:
		return storage.RemoteGCDropped, fmt.Errorf("point the remote's manifest at the collected table files: %w", err)
	}
	return storage.RemoteGCReplaced, nil
}

// storeBytesOf is the bytes of the table files the store's view holds.
func storeBytesOf(ctx context.Context, cs chunks.TableFileStore) (int64, error) {
	size, err := cs.Size(ctx)
	if err != nil {
		return 0, fmt.Errorf("measure the table files: %w", err)
	}
	return int64(size), nil
}

// openRemoteDB opens remote's database the way Dolt's SQL provider opens it
// for a push (env.Remote.GetRemoteDB, dolt libraries/doltcore/env/remotes.go):
// the URL and params dolt_remotes holds, the remote's name as the git
// remote's, and the git cache rooted where the provider roots it, so the open
// reads through the mirror every push and fetch keeps. Like the provider, it
// opens past dolt's in-process singleton cache: the caller owns the handle and
// closes it, so no cached entry is left serving a closed store.
func (s *Store) openRemoteDB(ctx context.Context, remote string) (*doltdb.DoltDB, error) {
	var remoteURL string
	var rawParams sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT url, params FROM dolt_remotes WHERE name = ?`, remote).Scan(&remoteURL, &rawParams); err != nil {
		return nil, fmt.Errorf("read remote %q: %w", remote, err)
	}
	params, err := decodeRemoteParams(remote, rawParams)
	if err != nil {
		return nil, err
	}
	params[dbfactory.GitRemoteNameParam] = remote
	params[dbfactory.GitCacheRootParam] = s.gitCacheRoot()
	params[dbfactory.DisableSingletonCacheParam] = struct{}{}
	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, remoteURL, filesys.LocalFS, params)
	if err != nil {
		return nil, fmt.Errorf("open remote %q (%s): %w", remote, remoteURL, err)
	}
	return remoteDB, nil
}
