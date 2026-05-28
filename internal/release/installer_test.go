package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTarGz returns a flat .tar.gz containing one regular-file entry per
// (name, content) pair. Mirrors goreleaser's archive shape so the installer's
// structural validation gets exercised against realistic inputs.
func buildTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(body)),
			ModTime:  time.Unix(0, 0),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newArchiveServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPInstallerAtomicReplacesBinary(t *testing.T) {
	archive := buildTarGz(t, map[string]string{
		"lit":     "NEW-BINARY",
		"LICENSE": "MIT",
	})
	srv := newArchiveServer(t, archive)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	if err := os.WriteFile(targetPath, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}

	tgt := &Target{
		Manifest: Manifest{},
		Artifact: Artifact{
			Platform: CurrentPlatform(),
			URL:      srv.URL + "/lit.tar.gz",
			SHA256:   sha256Hex(archive),
		},
	}
	inst := &HTTPInstaller{}
	if err := inst.Install(context.Background(), tgt, targetPath); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY" {
		t.Errorf("contents after install: got %q want NEW-BINARY", got)
	}

	// No leftover .tmp files in the target directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lit-downgrade-") {
			t.Errorf("orphan temp file left behind: %s", e.Name())
		}
	}
}

func TestHTTPInstallerSHA256MismatchRefuses(t *testing.T) {
	archive := buildTarGz(t, map[string]string{"lit": "NEW"})
	srv := newArchiveServer(t, archive)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	if err := os.WriteFile(targetPath, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	tgt := &Target{Artifact: Artifact{
		URL:    srv.URL + "/lit.tar.gz",
		SHA256: strings.Repeat("0", 64),
	}}
	inst := &HTTPInstaller{}
	err := inst.Install(context.Background(), tgt, targetPath)
	if err == nil {
		t.Fatal("expected SHA256 mismatch error")
	}
	if !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Errorf("error should name SHA256 mismatch: %v", err)
	}

	// Target unchanged.
	got, _ := os.ReadFile(targetPath)
	if string(got) != "OLD" {
		t.Errorf("target should be unchanged on checksum failure, got %q", got)
	}
	// No leftover temp.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lit-downgrade-") {
			t.Errorf("orphan temp file left behind: %s", e.Name())
		}
	}
}

func TestHTTPInstallerRejectsUnsafeArchiveEntry(t *testing.T) {
	// Path-traversal entry. extractLitBinary must refuse before writing.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("PWND"))
	_ = tw.Close()
	_ = gw.Close()
	archive := buf.Bytes()

	srv := newArchiveServer(t, archive)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	_ = os.WriteFile(targetPath, []byte("OLD"), 0o755)

	tgt := &Target{Artifact: Artifact{URL: srv.URL + "/x.tar.gz", SHA256: sha256Hex(archive)}}
	err := (&HTTPInstaller{}).Install(context.Background(), tgt, targetPath)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected unsafe-path rejection, got %v", err)
	}
}

func TestHTTPInstallerArchiveMissingBinary(t *testing.T) {
	archive := buildTarGz(t, map[string]string{"README": "hi"})
	srv := newArchiveServer(t, archive)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	_ = os.WriteFile(targetPath, []byte("OLD"), 0o755)

	tgt := &Target{Artifact: Artifact{URL: srv.URL + "/x.tar.gz", SHA256: sha256Hex(archive)}}
	err := (&HTTPInstaller{}).Install(context.Background(), tgt, targetPath)
	if err == nil || !strings.Contains(err.Error(), `"lit"`) {
		t.Fatalf("expected missing-binary error, got %v", err)
	}
}

// guard against an io.Reader being closed twice or similar regressions.
var _ io.Reader = (*bytes.Reader)(nil)
