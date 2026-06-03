package release

import (
	"archive/tar"
	"archive/zip"
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
	"time"
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

// BinaryName is the binary's stem inside the release archive — goreleaser
// writes a flat archive containing exactly this binary (suffixed .exe in the
// windows .zip) plus LICENSE / README files. scripts/install.sh extracts the
// same names; the constant is the consumer mirror of that producer convention.
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
	// [LAW:dataflow-not-control-flow] The archive format is data carried by the
	// artifact URL, not a mode the caller toggles. Deriving it up front (and
	// failing fast on an unknown extension) lets the extract stage below run
	// the same way for every platform, differing only in the enumerator the
	// format value supplies.
	format, err := archiveFormatForURL(target.Artifact.URL)
	if err != nil {
		return err
	}
	client := i.Client
	if client == nil {
		// Bounded default — http.DefaultClient is shared and has no Timeout.
		client = &http.Client{Timeout: defaultInstallerTimeout}
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

	if err := extractBinary(format, archive, tmp); err != nil {
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

// defaultInstallerTimeout bounds a single archive download end-to-end. The
// per-platform archives are ~15 MiB today; 5 minutes is generous on a slow
// link without allowing an indefinite hang. CLI passes context.Background()
// at time of writing, so without this bound a stalled server would wedge
// `lit downgrade` forever.
//
// [LAW:enumeration-gap] The accept shape of "an HTTP archive download"
// includes a deadline. http.DefaultClient has none; the boundary needs
// its own bounded default.
const defaultInstallerTimeout = 5 * time.Minute

// maxTotalUncompressedBytes bounds the SUM of bytes read from the gunzip
// stream while scanning the archive. The per-entry cap alone can't catch a
// many-small-entries bomb (N entries of cap-1 bytes each); a stream-level
// cap refuses that shape by construction. Set to 2x the per-entry cap so
// a legitimate `lit` + LICENSE + README archive is comfortably under it.
const maxTotalUncompressedBytes = 2 * maxUncompressedBytes

// boundedReader wraps a Reader and returns errStreamCap once total bytes
// read STRICTLY EXCEED cap. A stream of exactly cap bytes followed by a
// clean EOF passes through unchanged — that's the legitimate boundary.
// Used to refuse archive streams whose total uncompressed size would
// balloon past maxTotalUncompressedBytes regardless of per-entry sizes.
//
// [LAW:enumeration-gap] The implementation mirrors the compressed-bytes
// pattern in downloadAndVerify: allow reading up to cap+1 internally, so
// "exactly at the limit" and "over the limit" are mechanically
// distinguishable. A `>= cap` short-circuit would surface a spurious cap
// violation when the stream's true length is exactly cap.
type boundedReader struct {
	r    io.Reader
	cap  int64
	read int64
}

var errStreamCap = errors.New("release: uncompressed archive stream exceeds total cap")

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.read > b.cap {
		return 0, errStreamCap
	}
	// Allow reading one byte past cap so the next call's check can
	// distinguish "stream ended exactly at cap" from "stream had more."
	remaining := b.cap + 1 - b.read
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.cap {
		return n, errStreamCap
	}
	return n, err
}

// archiveFormat pairs a release archive's container format with the binary
// name its producer (goreleaser) writes into it. The two co-vary by goos:
// windows ships a .zip containing lit.exe; every other platform ships a
// .tar.gz containing lit. Binding the name to the format means the extractor
// never has to guess which entry is the binary — the format already knows.
type archiveFormat struct {
	binaryName string
	open       func(archive []byte) (archiveReader, error)
}

// archiveFormatForURL derives the format from the artifact URL suffix.
//
// [LAW:enumeration-gap] The accept shape of a release artifact URL is exactly
// {.tar.gz, .zip}; any other extension is refused here so the extractor can
// assume one of the two known shapes rather than mis-extracting.
func archiveFormatForURL(url string) (archiveFormat, error) {
	switch {
	case strings.HasSuffix(url, ".tar.gz"):
		return archiveFormat{binaryName: BinaryName, open: openTarGz}, nil
	case strings.HasSuffix(url, ".zip"):
		return archiveFormat{binaryName: BinaryName + ".exe", open: openZip}, nil
	default:
		return archiveFormat{}, fmt.Errorf("release: unsupported archive extension in %q (want .tar.gz or .zip)", url)
	}
}

// archiveReader enumerates the entries of a release archive in a format-
// independent way so the accept-shape lives in exactly one place.
//
// [LAW:single-enforcer] The path-safety, single-binary, and size-cap
// invariants are security-critical (zip-slip, silent-corruption, decompression
// bombs); duplicating them per container format would let them drift. The
// formats differ only in how entries are produced — that difference lives in
// the adapters below, the enforcement lives in extractBinary.
type archiveReader interface {
	// next advances to the following entry, returning io.EOF when exhausted.
	next() (archiveEntry, error)
	io.Closer
}

// archiveEntry is one member of a release archive, normalized across formats.
type archiveEntry struct {
	name    string
	regular bool
	size    int64 // declared uncompressed size
	// open yields the entry body. The caller invokes it only for the binary
	// entry and closes the result; for tar the body is valid only until the
	// next next() call, so extraction happens inline before advancing.
	open func() (io.ReadCloser, error)
}

type tarArchive struct {
	gzr *gzip.Reader
	tr  *tar.Reader
}

func openTarGz(archive []byte) (archiveReader, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("release: open gzip: %w", err)
	}
	// [LAW:enumeration-gap] The per-entry cap doesn't bound the sum across many
	// entries; this stream-level cap refuses a many-small-entries gzip bomb by
	// construction. Any tar Read() past the cap errors out.
	tr := tar.NewReader(&boundedReader{r: gzr, cap: maxTotalUncompressedBytes})
	return &tarArchive{gzr: gzr, tr: tr}, nil
}

func (a *tarArchive) next() (archiveEntry, error) {
	h, err := a.tr.Next()
	if err != nil {
		return archiveEntry{}, err // includes io.EOF
	}
	// tar.TypeRegA (NUL) is the historical alias for regular file; some writers
	// still emit it. Accept both so otherwise-valid archives pass.
	regular := h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA
	return archiveEntry{
		name:    h.Name,
		regular: regular,
		size:    h.Size,
		open:    func() (io.ReadCloser, error) { return io.NopCloser(a.tr), nil },
	}, nil
}

func (a *tarArchive) Close() error { return a.gzr.Close() }

type zipArchive struct {
	files []*zip.File
	idx   int
}

func openZip(archive []byte) (archiveReader, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("release: open zip: %w", err)
	}
	return &zipArchive{files: zr.File}, nil
}

func (a *zipArchive) next() (archiveEntry, error) {
	if a.idx >= len(a.files) {
		return archiveEntry{}, io.EOF
	}
	f := a.files[a.idx]
	a.idx++
	// A size beyond int64 wraps negative here; the < 0 guard in extractBinary
	// rejects it, so a lying central-directory size can't slip past the cap.
	return archiveEntry{
		name:    f.Name,
		regular: f.Mode().IsRegular(),
		size:    int64(f.UncompressedSize64),
		open:    f.Open,
	}, nil
}

func (a *zipArchive) Close() error { return nil }

// extractBinary applies the release-archive accept shape and writes the single
// binary entry to dest. The accept shape is intentionally tighter than
// scripts/install.sh's: it rejects any filename containing "..", rejects
// backslashes as well as forward slashes, requires exactly one
// format.binaryName entry, and caps the uncompressed size of every entry.
// install.sh applies the path checks but not the uncompressed cap; the Go path
// is stricter by design at the resolver→installer boundary.
//
// [LAW:types-are-the-program] The accept shape is "flat archive of regular
// files containing one entry named <binaryName>, each ≤ maxUncompressedBytes";
// reject the rest by construction so the rest of Install can assume safe input.
func extractBinary(format archiveFormat, archive []byte, dest io.Writer) error {
	ar, err := format.open(archive)
	if err != nil {
		return err
	}
	defer ar.Close()

	found := false
	for {
		e, err := ar.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("release: read archive: %w", err)
		}
		if !safeFlatName(e.name) {
			return fmt.Errorf("release: archive entry has unsafe path: %q", e.name)
		}
		if !e.regular {
			return fmt.Errorf("release: archive contains non-regular entry %q", e.name)
		}
		// Refuse any entry — target or not — that declares more than the cap:
		// a hostile LICENSE could be a decompression bomb even though we never
		// copy it. The header check is cheap; refuse before any body read.
		if e.size < 0 || e.size > maxUncompressedBytes {
			return fmt.Errorf("release: archive entry %q declares %d uncompressed bytes (cap %d)", e.name, e.size, maxUncompressedBytes)
		}
		if e.name != format.binaryName {
			continue
		}
		if found {
			// [LAW:types-are-the-program] "exactly one binary entry" is the
			// type-level claim above; a second entry can't be allowed to
			// silently corrupt the extracted binary by appending.
			return fmt.Errorf("release: archive contains multiple %q entries", format.binaryName)
		}
		if err := copyCappedEntry(dest, e); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("release: archive did not contain a %q entry", format.binaryName)
	}
	return nil
}

// copyCappedEntry streams e's body to dest, refusing a body whose actual size
// exceeds the cap even when the declared header size was within it.
func copyCappedEntry(dest io.Writer, e archiveEntry) error {
	body, err := e.open()
	if err != nil {
		return fmt.Errorf("release: open entry %q: %w", e.name, err)
	}
	defer body.Close()
	// [LAW:enumeration-gap] CopyN bounds the actual bytes streamed even if the
	// header size lied. The +1 distinguishes "exactly at the cap" from
	// "overflow," matching the compressed-byte handling in downloadAndVerify.
	n, err := io.CopyN(dest, body, maxUncompressedBytes+1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("release: extract %s: %w", e.name, err)
	}
	if n > maxUncompressedBytes {
		return fmt.Errorf("release: %q exceeded uncompressed cap %d", e.name, maxUncompressedBytes)
	}
	return nil
}

// safeFlatName reports whether name is a single flat path component safe to
// extract — no traversal, no separators of either slash direction.
func safeFlatName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}
