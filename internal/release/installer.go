package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Installer downloads, verifies, and atomically installs the binary the Target
// describes.
//
// [LAW:single-enforcer] Installer owns the fetch-and-swap path. No other code
// in the codebase performs binary self-replacement.
// [LAW:dataflow-not-control-flow] Install runs the same stages every call
// (download → SHA256 → structural check → extract → atomic rename); failure
// at any stage stops the pipeline. Variability is in the Target value, not
// in flags that toggle stages.
type Installer interface {
	Install(ctx context.Context, target *Target, targetPath string) error
}

// BinaryName is the file inside the release archive that gets installed —
// goreleaser writes a flat archive containing exactly this binary plus
// LICENSE / README files. scripts/install.sh extracts the same name; the
// constant is the consumer mirror of that producer convention.
const BinaryName = "lit"

// HTTPInstaller is the default Installer.
type HTTPInstaller struct {
	Client *http.Client // nil defaults to http.DefaultClient
}

// Install fetches target.Artifact.URL, verifies it against target.Artifact.SHA256,
// extracts BinaryName from the archive into a sibling temp file on the same
// filesystem as targetPath, and atomically renames it into place.
//
// On any failure before the rename, targetPath is unchanged and the temp file
// is removed.
func (i *HTTPInstaller) Install(ctx context.Context, target *Target, targetPath string) error {
	if target == nil {
		return errors.New("release: nil target")
	}
	if !strings.HasSuffix(target.Artifact.URL, ".tar.gz") {
		// [LAW:types-are-the-program] goreleaser writes .tar.gz for every
		// platform lit currently ships (.zip would only appear if windows
		// builds came back); reject anything else at the boundary so the
		// extract path can assume tar.gz.
		return fmt.Errorf("release: unsupported archive extension in %q (want .tar.gz)", target.Artifact.URL)
	}
	client := i.Client
	if client == nil {
		client = http.DefaultClient
	}

	archive, err := downloadAndVerify(ctx, client, target.Artifact)
	if err != nil {
		return err
	}

	// Temp file on the same filesystem as targetPath so os.Rename is genuinely
	// atomic. Cross-FS rename falls back to copy+unlink, which is not atomic.
	targetDir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(targetDir, ".lit-downgrade-*.tmp")
	if err != nil {
		return fmt.Errorf("release: create temp file in %s: %w", targetDir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := extractLitBinary(archive, tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("release: chmod temp binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("release: close temp binary: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("release: atomic rename into %s: %w", targetPath, err)
	}
	committed = true
	return nil
}

// downloadAndVerify fetches the archive, hashing as it reads, and returns the
// bytes only when the digest matches Artifact.SHA256. Verifying-as-we-read
// avoids a second pass and keeps the failure point as early as possible.
func downloadAndVerify(ctx context.Context, client *http.Client, a Artifact) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: fetch %s: %w", a.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: fetch %s: HTTP %d", a.URL, resp.StatusCode)
	}
	h := sha256.New()
	body, err := io.ReadAll(io.TeeReader(resp.Body, h))
	if err != nil {
		return nil, fmt.Errorf("release: read %s: %w", a.URL, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != a.SHA256 {
		return nil, fmt.Errorf("release: SHA256 mismatch for %s: expected %s, got %s", a.URL, a.SHA256, actual)
	}
	return body, nil
}

// extractLitBinary scans the tar.gz for a flat `lit` entry of regular-file
// type and writes its contents to dest. Mirrors scripts/install.sh's
// structural validation: reject path-traversal, reject non-regular entries
// (symlinks, devices, hardlinks), and require exactly one entry named
// BinaryName. Other flat-regular entries (LICENSE, README) are tolerated and
// skipped.
//
// [LAW:types-are-the-program] The accept shape is "flat archive of regular
// files containing one entry named lit"; reject the rest by construction so
// the rest of Install can assume safe inputs.
func extractLitBinary(archive []byte, dest io.Writer) error {
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("release: open gzip: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("release: read tar: %w", err)
		}
		name := h.Name
		if name == "" || name == "." || name == ".." ||
			strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			return fmt.Errorf("release: archive entry has unsafe path: %q", name)
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("release: archive contains non-regular entry %q (type %c)", name, h.Typeflag)
		}
		if name != BinaryName {
			continue
		}
		if _, err := io.Copy(dest, tr); err != nil {
			return fmt.Errorf("release: extract %s: %w", BinaryName, err)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("release: archive did not contain a %q entry", BinaryName)
	}
	return nil
}
