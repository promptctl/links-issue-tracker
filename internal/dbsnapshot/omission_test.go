package dbsnapshot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// doltStorageTree lays down, under a fresh root, the shape of a Dolt storage
// directory with every path an omission names: the data, Dolt's journal
// LOCK, a remote's mirror in the remote cache and a temp table file.
func doltStorageTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dolt")
	files := map[string]string{
		"links/.dolt/noms/manifest":                                "m",
		"links/.dolt/noms/LOCK":                                    "",
		"links/.dolt/repo_state.json":                              "{}",
		"links/.dolt/" + RemoteCacheDirName + "/key/repo.git/HEAD": "ref",
		"links/.dolt/" + TempTableFilesDirName + "/buffered_sink":  "tmp",
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestCloneTreeLeavesOutWhatItsOmissionNames runs each omission through this
// platform's own cloneTree — Darwin's Clonefile path on a Mac, the walk on
// Linux — and reads what the copy holds. A snapshot keeps the data and drops
// the LOCK, the remote cache and the temp files; a clone keeps the cache and
// temp files it is fetched and pushed from, and drops only the LOCK.
func TestCloneTreeLeavesOutWhatItsOmissionNames(t *testing.T) {
	cases := []struct {
		name    string
		omit    omission
		held    []string
		leftOut []string
	}{
		{
			name:    "snapshot",
			omit:    snapshotOmits,
			held:    []string{"links/.dolt/noms/manifest", "links/.dolt/repo_state.json"},
			leftOut: []string{"links/.dolt/noms/LOCK", "links/.dolt/" + RemoteCacheDirName, "links/.dolt/" + TempTableFilesDirName},
		},
		{
			name:    "clone",
			omit:    cloneOmits,
			held:    []string{"links/.dolt/noms/manifest", "links/.dolt/" + RemoteCacheDirName + "/key/repo.git/HEAD", "links/.dolt/" + TempTableFilesDirName + "/buffered_sink"},
			leftOut: []string{"links/.dolt/noms/LOCK"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := doltStorageTree(t)
			dst := filepath.Join(t.TempDir(), "copy")
			if err := cloneTree(context.Background(), src, dst, tc.omit); err != nil {
				t.Fatalf("cloneTree() error = %v", err)
			}
			for _, rel := range tc.held {
				if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); err != nil {
					t.Errorf("%s missing from the copy: %v", rel, err)
				}
			}
			for _, rel := range tc.leftOut {
				if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s is in the copy (stat err = %v); the omission leaves it out", rel, err)
				}
			}
		})
	}
}

// TestWalkAndCopyLeavesOutWhatItsOmissionNames pins the walk on every
// platform, Darwin included, where the test above exercises Clonefile
// instead: an omitted directory is skipped whole and an omitted file is not
// copied, while their siblings are.
func TestWalkAndCopyLeavesOutWhatItsOmissionNames(t *testing.T) {
	src := doltStorageTree(t)
	dst := filepath.Join(t.TempDir(), "copy")
	if err := walkAndCopy(context.Background(), src, dst, snapshotOmits, plainFileCopy); err != nil {
		t.Fatalf("walkAndCopy() error = %v", err)
	}
	for _, rel := range []string{"links/.dolt/noms/LOCK", "links/.dolt/" + RemoteCacheDirName, "links/.dolt/" + TempTableFilesDirName} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is in the copy (stat err = %v); the walk must leave it out", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "links", ".dolt", "noms", "manifest")); err != nil {
		t.Errorf("manifest, a sibling of the omitted LOCK, missing from the copy: %v", err)
	}
}
