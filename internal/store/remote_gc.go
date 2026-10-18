package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

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
// It writes, in a scratch store, the table files reachable from head named as
// branch and nothing else (buildCollectedSet), and then replaces the remote's
// table files with them (replaceRemoteTableFiles). The scratch goes however
// that ended.
func (s *Store) collectRemote(ctx context.Context, remote, branch, head string) (gc storage.RemoteGC, err error) {
	scratch, err := os.MkdirTemp("", "lit-remote-gc-")
	if err != nil {
		return gc, fmt.Errorf("create the remote GC's scratch store: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	collected, err := s.buildCollectedSet(ctx, scratch, branch, head)
	if err != nil {
		return gc, err
	}
	defer func() { err = errors.Join(err, collected.Close()) }()
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		var err error
		gc, err = s.replaceRemoteTableFiles(ctx, remote, branch, head, collected)
		return err
	})
	return gc, err
}

// buildCollectedSet opens a fresh store in dir holding the chunks reachable
// from head and a root that names head as branch and nothing else: the root
// the remote holds once its other branches are gone, and table files holding
// everything that root reaches. The store has no chunk journal, so every
// chunk it holds is in a table file the remote can take.
func (s *Store) buildCollectedSet(ctx context.Context, dir, branch, head string) (*doltdb.DoltDB, error) {
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
	err = s.withEngineClosed(ctx, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		return collected.PullChunks(holdCtx, tempDir, ddb, []hash.Hash{addr}, nil, nil)
	})
	if err == nil {
		err = collected.SetHead(ctx, ref.NewBranchRef(branch), addr)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("copy what %s reaches into the remote GC's scratch store: %w", head, err), collected.Close())
	}
	return collected, nil
}

// replaceRemoteTableFiles is the remote's GC proper, measured on either side
// (collectAtHead). The caller holds the commit lock, so no other writer in
// this checkout reaches the remote meanwhile.
func (s *Store) replaceRemoteTableFiles(ctx context.Context, remote, branch, head string, collected *doltdb.DoltDB) (gc storage.RemoteGC, err error) {
	remoteDB, err := s.openRemoteDB(ctx, remote)
	if err != nil {
		return gc, err
	}
	defer func() { err = errors.Join(err, remoteDB.Close()) }()
	// [LAW:parse-dont-validate] the one shape the GC can replace: a git
	// remote's store is a single NomsBlockStore over its blobstore.
	remoteCS, ok := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(remoteDB)).(*nbs.NomsBlockStore)
	if !ok {
		return gc, fmt.Errorf("remote %q's store is a %T; the remote's GC replaces the table files of a git remote's store", remote, datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(remoteDB)))
	}
	collectedCS, ok := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(collected)).(chunks.TableFileStore)
	if !ok {
		return gc, fmt.Errorf("the remote GC's scratch store is a %T, which lists no table files", datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(collected)))
	}

	if gc.FilesBefore, gc.BytesBefore, err = tableSetOf(ctx, remoteCS); err != nil {
		return gc, err
	}
	if gc.Outcome, gc.DroppedBranches, err = collectAtHead(ctx, remoteDB, remoteCS, collectedCS, branch, head); err != nil {
		return gc, err
	}
	gc.FilesAfter, gc.BytesAfter, err = tableSetOf(ctx, remoteCS)
	return gc, err
}

// collectAtHead runs the GC's writes in the order that leaves a remote a
// peer has written to whole. Unless the remote's branch is still at head it
// ends RemoteGCMoved having written nothing. Then it deletes every other
// branch of the remote, which the collected files do not hold, and then,
// while the remote's root is the one collected holds, writes collected's
// table files to the remote and points its manifest at exactly those, under
// a new GC generation (nbs.ReplaceStoreTableFiles, dolt patch 10). The new
// generation is what keeps a writer whose view predates the GC from landing
// table files that reference a chunk it dropped: that writer re-checks them
// against the new files first.
//
// The remote's root is compared before the upload, and the manifest write
// compares it again: a write that lands on the remote after the head check
// moves the manifest's lock, and either check ends the GC as RemoteGCMoved
// with the remote's table set whole.
func collectAtHead(ctx context.Context, remoteDB *doltdb.DoltDB, remote *nbs.NomsBlockStore, collected chunks.TableFileStore, branch, head string) (storage.RemoteGCOutcome, int, error) {
	refs, err := remoteDB.GetRefsWithHashes(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("list the remote's refs: %w", err)
	}
	keep := ref.NewBranchRef(branch)
	// [LAW:no-ambient-temporal-coupling] the head check owns "a peer wrote
	// first": it runs before the GC's first write, so a moved remote is never
	// half collected.
	if !slices.ContainsFunc(refs, func(r doltdb.RefWithHash) bool { return ref.Equals(r.Ref, keep) && r.Hash.String() == head }) {
		return storage.RemoteGCMoved, 0, nil
	}
	dropped, err := dropOtherBranches(ctx, remoteDB, refs, keep)
	if err != nil {
		return "", dropped, err
	}
	outcome, err := replaceWhileRootHolds(ctx, collected, remote)
	return outcome, dropped, err
}

// replaceWhileRootHolds writes collected's table files to the remote and
// points the remote's manifest at them, while the remote's root is
// collected's.
func replaceWhileRootHolds(ctx context.Context, collected chunks.TableFileStore, remote *nbs.NomsBlockStore) (storage.RemoteGCOutcome, error) {
	sources, err := collected.Sources(ctx)
	if err != nil {
		return "", fmt.Errorf("list the collected table files: %w", err)
	}
	remoteRoot, err := remote.Root(ctx)
	if err != nil {
		return "", fmt.Errorf("read the remote's root: %w", err)
	}
	if remoteRoot != sources.Root {
		return storage.RemoteGCMoved, nil
	}
	files, err := uploadTableFiles(ctx, sources, remote)
	if err != nil {
		return "", err
	}
	err = nbs.ReplaceStoreTableFiles(ctx, remote, files)
	switch {
	case errors.Is(err, nbs.ErrConcurrentManifestWriteDuringOverwrite):
		return storage.RemoteGCMoved, nil
	case err != nil:
		return "", fmt.Errorf("point the remote's manifest at the collected table files: %w", err)
	}
	return storage.RemoteGCReplaced, nil
}

// uploadTableFiles writes every table file of sources to the remote, as
// Dolt's clone writes a table file to its sink, and returns them as the
// table set the remote's manifest takes. A chunk journal or an appendix is
// not a shape the collected store writes, so either one is an error rather
// than a file left behind.
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

// dropOtherBranches deletes every branch of refs but keep and returns how
// many it deleted. Anything refs names that is not a branch is an error
// before the first delete: the collected files hold keep's history alone,
// and the GC drops nothing until it can name everything it drops.
func dropOtherBranches(ctx context.Context, remoteDB *doltdb.DoltDB, refs []doltdb.RefWithHash, keep ref.DoltRef) (int, error) {
	var drop []ref.DoltRef
	for _, r := range refs {
		switch {
		case ref.Equals(r.Ref, keep):
		case r.Ref.GetType() == ref.BranchRefType:
			drop = append(drop, r.Ref)
		default:
			return 0, fmt.Errorf("the remote holds %s, which is not a branch the remote's GC can drop", r.Ref.String())
		}
	}
	for i, r := range drop {
		if err := remoteDB.DeleteBranch(ctx, r, nil); err != nil {
			return i, fmt.Errorf("delete the remote's branch %s: %w", r.GetPath(), err)
		}
	}
	return len(drop), nil
}

// tableSetOf is the table files the store's manifest names, and their bytes.
func tableSetOf(ctx context.Context, cs *nbs.NomsBlockStore) (int, int64, error) {
	sources, err := cs.Sources(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("list the remote's table files: %w", err)
	}
	size, err := cs.Size(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("measure the remote's table files: %w", err)
	}
	return len(sources.TableFiles), int64(size), nil
}

// openRemoteDB opens remote's database the way Dolt's SQL provider opens it
// for a push (env.Remote.GetRemoteDB, dolt libraries/doltcore/env/remotes.go):
// the URL and params dolt_remotes holds, the remote's name as the git
// remote's, and the git cache rooted where the provider roots it, so the open
// reads through the mirror every push and fetch keeps.
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
	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, remoteURL, filesys.LocalFS, params)
	if err != nil {
		return nil, fmt.Errorf("open remote %q (%s): %w", remote, remoteURL, err)
	}
	return remoteDB, nil
}
