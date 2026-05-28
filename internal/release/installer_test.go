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

func TestHTTPInstallerRejectsMultipleLitEntries(t *testing.T) {
	// Two `lit` entries — extractLitBinary must refuse the second, not append.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, body := range []string{"FIRST", "SECOND"} {
		_ = tw.WriteHeader(&tar.Header{Name: "lit", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gw.Close()
	archive := buf.Bytes()

	srv := newArchiveServer(t, archive)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	_ = os.WriteFile(targetPath, []byte("OLD"), 0o755)

	tgt := &Target{Artifact: Artifact{URL: srv.URL + "/x.tar.gz", SHA256: sha256Hex(archive)}}
	err := (&HTTPInstaller{}).Install(context.Background(), tgt, targetPath)
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("expected multiple-entry rejection, got %v", err)
	}
}

func TestHTTPInstallerAcceptsTypeRegA(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := "NEW"
	_ = tw.WriteHeader(&tar.Header{Name: "lit", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeRegA})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gw.Close()
	archive := buf.Bytes()

	srv := newArchiveServer(t, archive)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	_ = os.WriteFile(targetPath, []byte("OLD"), 0o755)

	tgt := &Target{Artifact: Artifact{URL: srv.URL + "/x.tar.gz", SHA256: sha256Hex(archive)}}
	if err := (&HTTPInstaller{}).Install(context.Background(), tgt, targetPath); err != nil {
		t.Fatalf("Install with TypeRegA: %v", err)
	}
	got, _ := os.ReadFile(targetPath)
	if string(got) != "NEW" {
		t.Errorf("contents: got %q want NEW", got)
	}
}

func TestHTTPInstallerRejectsOversizedEntryHeader(t *testing.T) {
	// Tar header claims an uncompressed size above the cap; the body is tiny
	// (a gzip-bomb shape). The header check rejects it before streaming a
	// single body byte.
	var bomb bytes.Buffer
	gw := gzip.NewWriter(&bomb)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{
		Name:     "lit",
		Mode:     0o755,
		Size:     int64(maxUncompressedBytes) + 1,
		Typeflag: tar.TypeReg,
	})
	// Write a few real bytes; the cap check fires on the header Size, not body.
	_, _ = tw.Write([]byte("tiny"))
	_ = tw.Close()
	_ = gw.Close()
	archive := bomb.Bytes()

	srv := newArchiveServer(t, archive)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	_ = os.WriteFile(targetPath, []byte("OLD"), 0o755)

	tgt := &Target{Artifact: Artifact{URL: srv.URL + "/x.tar.gz", SHA256: sha256Hex(archive)}}
	err := (&HTTPInstaller{}).Install(context.Background(), tgt, targetPath)
	if err == nil || !strings.Contains(err.Error(), "uncompressed") {
		t.Fatalf("expected uncompressed-cap rejection, got %v", err)
	}
}

func TestBoundedReaderEdgeCases(t *testing.T) {
	// Exactly cap bytes followed by EOF must pass through cleanly.
	exact := bytes.NewReader(make([]byte, 100))
	br := &boundedReader{r: exact, cap: 100}
	buf := make([]byte, 200)
	n, err := br.Read(buf)
	if n != 100 {
		t.Fatalf("first read n=%d, want 100", n)
	}
	// err can be nil or io.EOF here depending on bytes.Reader's behavior;
	// what matters is that the second read returns clean EOF, not errStreamCap.
	if err != nil && err != io.EOF {
		t.Fatalf("first read unexpected err: %v", err)
	}
	n, err = br.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("at-cap second read = (%d, %v); want (0, io.EOF)", n, err)
	}

	// One byte past cap must surface errStreamCap.
	over := bytes.NewReader(make([]byte, 101))
	br2 := &boundedReader{r: over, cap: 100}
	// Drain via small reads to exercise the overflow check.
	total := 0
	for {
		n, err := br2.Read(buf[:50])
		total += n
		if err == errStreamCap {
			break
		}
		if err == io.EOF {
			t.Fatalf("expected errStreamCap, got io.EOF after %d bytes", total)
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if total < 100 {
		t.Errorf("expected to read at least cap bytes before overflow; got %d", total)
	}
}

func TestHTTPInstallerEnforcesSizeCap(t *testing.T) {
	// Server streams strictly more than maxArchiveBytes (in 1 MiB chunks); the
	// installer must refuse with the size-cap error before SHA256 verification.
	// The exact overflow amount doesn't matter — any read past the cap trips
	// the explicit overflow check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zero := make([]byte, 1<<20)
		for written := 0; written <= maxArchiveBytes; written += len(zero) {
			_, _ = w.Write(zero)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "lit")
	_ = os.WriteFile(targetPath, []byte("OLD"), 0o755)

	tgt := &Target{Artifact: Artifact{URL: srv.URL + "/x.tar.gz", SHA256: strings.Repeat("0", 64)}}
	err := (&HTTPInstaller{}).Install(context.Background(), tgt, targetPath)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size-cap refusal, got %v", err)
	}
}

