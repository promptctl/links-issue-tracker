package dbsnapshot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// doltStorageTree lays down, under a fresh root, the shape of a Dolt storage
// directory with every path an omission names: the data, Dolt's journal
// LOCK, the stats sidecar's own LOCK one database deeper, a remote's mirror
// in the remote cache and a temp table file.
func doltStorageTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dolt")
	files := map[string]string{
		"links/.dolt/noms/manifest":                                "m",
		"links/.dolt/noms/LOCK":                                    "",
		"links/.dolt/repo_state.json":                              "{}",
		"links/.dolt/stats/.dolt/noms/manifest":                    "m",
		"links/.dolt/stats/.dolt/noms/LOCK":                        "",
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

// TestEveryCopyEngineLeavesOutWhatItsOmissionNames runs each omission through
// every engine a copy can take — this platform's cloneTree, the walk, and the
// whole copy Darwin's Clonefile makes followed by removeFrom — on every
// platform, and reads what the copy holds and what the engine reports it left
// out. A snapshot keeps the data and leaves out both LOCKs, the remote cache
// and the temp files; a clone keeps the cache and temp files it is fetched and
// pushed from, and leaves out only the LOCKs. The copy's path holds a glob
// metacharacter, which no engine may read as one.
func TestEveryCopyEngineLeavesOutWhatItsOmissionNames(t *testing.T) {
	ctx := context.Background()
	engines := []struct {
		name string
		copy func(src, dst string, omit omission) ([]string, error)
	}{
		{"cloneTree", func(src, dst string, omit omission) ([]string, error) {
			return cloneTree(ctx, src, dst, omit)
		}},
		{"walkAndCopy", func(src, dst string, omit omission) ([]string, error) {
			return walkAndCopy(ctx, src, dst, omit, plainFileCopy)
		}},
		{"removeFrom", func(src, dst string, omit omission) ([]string, error) {
			if _, err := walkAndCopy(ctx, src, dst, nil, plainFileCopy); err != nil {
				return nil, err
			}
			return omit.removeFrom(dst)
		}},
	}
	cases := []struct {
		name    string
		omit    omission
		held    []string
		leftOut []string
	}{
		{
			name: "snapshot",
			omit: snapshotOmits,
			held: []string{"links/.dolt/noms/manifest", "links/.dolt/repo_state.json", "links/.dolt/stats/.dolt/noms/manifest"},
			leftOut: []string{
				"links/.dolt/" + RemoteCacheDirName,
				"links/.dolt/noms/LOCK",
				"links/.dolt/stats/.dolt/noms/LOCK",
				"links/.dolt/" + TempTableFilesDirName,
			},
		},
		{
			name:    "clone",
			omit:    cloneOmits,
			held:    []string{"links/.dolt/noms/manifest", "links/.dolt/" + RemoteCacheDirName + "/key/repo.git/HEAD", "links/.dolt/" + TempTableFilesDirName + "/buffered_sink"},
			leftOut: []string{"links/.dolt/noms/LOCK", "links/.dolt/stats/.dolt/noms/LOCK"},
		},
	}
	for _, engine := range engines {
		for _, tc := range cases {
			t.Run(engine.name+"/"+tc.name, func(t *testing.T) {
				src := doltStorageTree(t)
				dst := filepath.Join(t.TempDir(), "copy[1")
				leftOut, err := engine.copy(src, dst, tc.omit)
				if err != nil {
					t.Fatalf("copy error = %v", err)
				}
				if !slices.Equal(leftOut, tc.leftOut) {
					t.Errorf("left out %q, want %q", leftOut, tc.leftOut)
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
}
