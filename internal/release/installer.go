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

// maxArchiveBytes bounds the in-memory archive size the installer accepts.
// Current per-platform archives are ~10MB; 256MiB is generous headroom while
// still refusing pathological responses that could OOM a workstation.
//
// [LAW:enumeration-gap] The accept-shape of "this is a release archive"
// includes a size bound; without it any HTTP body — including a hostile one
// — would flow into ReadAll. Refuse oversized inputs at the boundary.
const maxArchiveBytes = 256 << 20

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

	// [LAW:dataflow-not-control-flow] Create the temp file BEFORE downloading
	// so a non-writable install dir (e.g. /usr/local/bin without sudo) fails
	// fast and doesn't burn bandwidth fetching an archive we couldn't have
	// installed anyway. Temp file lives on the same filesystem as targetPath
	// so the eventual os.Rename is genuinely atomic.
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

	archive, err := downloadAndVerify(ctx, client, target.Artifact)
	if err != nil {
		_ = tmp.Close()
		return err
	}

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
		// [LAW:one-source-of-truth] Same diagnostic shape as resolver.Resolve
		// — include a size-limited body snippet so operators can distinguish
		// GitHub's "Not Found" page from auth/ratelimit HTML, etc.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("release: fetch %s: HTTP %d: %s", a.URL, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// [LAW:types-are-the-program] Decode the expected SHA256 BEFORE downloading
	// so a malformed manifest digest fails fast (and never lets us trust a
	// post-download comparison that would silently never match). The expected
	// digest is a 32-byte value; the hex string is one representation. Decoding
	// to bytes also makes the comparison case-insensitive by construction —
	// hex.DecodeString accepts both cases.
	expected, err := hex.DecodeString(a.SHA256)
	if err != nil || len(expected) != sha256.Size {
		return nil, fmt.Errorf("release: artifact SHA256 %q is not a 64-char hex digest", a.SHA256)
	}
	h := sha256.New()
	// [LAW:enumeration-gap] LimitReader caps the body at maxArchiveBytes+1 so
	// we can distinguish "exactly at the limit" from "exceeded": if ReadAll
	// returns maxArchiveBytes+1 bytes, the source overflowed the cap.
	limited := io.LimitReader(resp.Body, maxArchiveBytes+1)
	body, err := io.ReadAll(io.TeeReader(limited, h))
	if err != nil {
		return nil, fmt.Errorf("release: read %s: %w", a.URL, err)
	}
	if int64(len(body)) > maxArchiveBytes {
		return nil, fmt.Errorf("release: archive %s exceeds %d byte cap", a.URL, maxArchiveBytes)
	}
	if actual := h.Sum(nil); !bytes.Equal(actual, expected) {
		return nil, fmt.Errorf("release: SHA256 mismatch for %s: expected %s, got %s", a.URL, a.SHA256, hex.EncodeToString(actual))
	}
	return body, nil
}

// maxUncompressedBytes bounds the post-gunzip bytes any single entry may
// expand to. The `lit` binary is ~15 MiB today; 256 MiB is generous headroom
// while refusing gzip bombs that fit under the compressed download cap but
// expand to gigabytes.
//
// [LAW:enumeration-gap] The compressed-bytes cap (maxArchiveBytes) doesn't
// bound expansion; the trust-boundary accept shape must include the
// uncompressed bound too, or a 1 MiB tar.gz of zeros could fill the disk.
const maxUncompressedBytes = 256 << 20

// maxTotalUncompressedBytes bounds the SUM of bytes read from the gunzip
// stream while scanning the archive. The per-entry cap alone can't catch a
// many-small-entries bomb (N entries of cap-1 bytes each); a stream-level
// cap refuses that shape by construction. Set to 2x the per-entry cap so
// a legitimate `lit` + LICENSE + README archive is comfortably under it.
const maxTotalUncompressedBytes = 2 * maxUncompressedBytes

// boundedReader wraps a Reader and returns errStreamCap once total bytes
// read exceed cap. Used to refuse archive streams whose total uncompressed
// size would balloon past maxTotalUncompressedBytes regardless of per-entry
// sizes.
type boundedReader struct {
	r     io.Reader
	cap   int64
	read  int64
}

var errStreamCap = errors.New("release: uncompressed archive stream exceeds total cap")

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.read >= b.cap {
		return 0, errStreamCap
	}
	if int64(len(p)) > b.cap-b.read {
		p = p[:b.cap-b.read]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

// extractLitBinary scans the tar.gz for a flat `lit` entry of regular-file
// type and writes its contents to dest. The accept shape is intentionally
// tighter than scripts/install.sh's: this implementation rejects any
// filename containing "..", rejects backslashes as well as forward slashes,
// requires exactly one BinaryName entry, and caps the uncompressed size of
// the extracted entry. install.sh applies the same first two checks via
// shell case-patterns but does not enforce the uncompressed cap; the Go
// path is stricter by design at the resolver→installer boundary.
//
// [LAW:types-are-the-program] The accept shape is "flat archive of regular
// files containing one entry named lit, of size ≤ maxUncompressedBytes";
// reject the rest by construction so the rest of Install can assume safe
// inputs.
func extractLitBinary(archive []byte, dest io.Writer) error {
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("release: open gzip: %w", err)
	}
	defer gzr.Close()
	// [LAW:enumeration-gap] The per-entry cap doesn't bound the sum across
	// many entries; this stream-level cap refuses a many-small-entries gzip
	// bomb by construction. Any tar Read() past the cap errors out.
	tr := tar.NewReader(&boundedReader{r: gzr, cap: maxTotalUncompressedBytes})
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
		// tar.TypeRegA (NUL) is the historical alias for regular file; some
		// writers still emit it. Accept both so otherwise-valid archives pass.
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return fmt.Errorf("release: archive contains non-regular entry %q (type %c)", name, h.Typeflag)
		}
		if name != BinaryName {
			// Even for non-target entries, refuse a claimed size beyond the
			// cap — a malicious archive that declares a huge LICENSE could
			// be a gzip bomb in disguise even though we wouldn't copy it.
			// Header inspection alone is cheap; the cost lives in Copy.
			if h.Size > maxUncompressedBytes {
				return fmt.Errorf("release: archive entry %q declares %d uncompressed bytes (cap %d)", name, h.Size, maxUncompressedBytes)
			}
			continue
		}
		if found {
			// [LAW:types-are-the-program] "exactly one lit entry" is the
			// type-level claim the comment above makes; enforce it here so
			// a second entry can't silently corrupt the extracted binary by
			// appending.
			return fmt.Errorf("release: archive contains multiple %q entries", BinaryName)
		}
		// Reject the declared size before streaming; a header-only check
		// avoids reading the body when it would have failed the cap anyway.
		if h.Size < 0 || h.Size > maxUncompressedBytes {
			return fmt.Errorf("release: %q declares %d uncompressed bytes (cap %d)", BinaryName, h.Size, maxUncompressedBytes)
		}
		// [LAW:enumeration-gap] CopyN bounds the actual bytes streamed even
		// if the tar header lies. The +1 lets us distinguish "exactly at the
		// cap" from "overflow," matching the compressed-byte handling above.
		n, err := io.CopyN(dest, tr, maxUncompressedBytes+1)
		if err != nil && err != io.EOF {
			return fmt.Errorf("release: extract %s: %w", BinaryName, err)
		}
		if n > maxUncompressedBytes {
			return fmt.Errorf("release: %q exceeded uncompressed cap %d", BinaryName, maxUncompressedBytes)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("release: archive did not contain a %q entry", BinaryName)
	}
	return nil
}
