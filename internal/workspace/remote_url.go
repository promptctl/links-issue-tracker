package workspace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// CheckoutRoot is a checkout's top-level directory as an absolute path with
// every symlink resolved, the form git prints for `--show-toplevel`. A
// relative remote URL is joined to it without touching the filesystem, and a
// lexical join of a leading ".." gives the OS's answer only from a root with
// no symlink left in it. The zero value names no checkout: a Remotes over it
// fails every read at gitStdout, and a GitRemote over it panics when its URL
// is a path it would join to it.
// [LAW:types-are-the-program]
type CheckoutRoot struct {
	path string
}

func (r CheckoutRoot) String() string { return r.path }

// ParseCheckoutRoot is how a root that did not come from git's own answer
// becomes one: it refuses a path that is not absolute, or that is not already
// its own symlink resolution. [LAW:parse-dont-validate]
func ParseCheckoutRoot(path string) (CheckoutRoot, error) {
	if !filepath.IsAbs(path) {
		return CheckoutRoot{}, fmt.Errorf("checkout root %q is not absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return CheckoutRoot{}, fmt.Errorf("resolve checkout root %q: %w", path, err)
	}
	if resolved != path {
		return CheckoutRoot{}, fmt.Errorf("checkout root %q is not its own symlink resolution %q", path, resolved)
	}
	return CheckoutRoot{path: path}, nil
}

// GitRemote is one remote of git's remote list: its name as git spells it,
// its fetch URL as written, and the checkout root it was listed in, which
// together say what git reads the URL as. NewGitRemote is the only way to
// build one. [LAW:parse-dont-validate]
type GitRemote struct {
	name    string
	written string
	root    CheckoutRoot
}

// NewGitRemote is the remote git lists with the fetch URL written, as the
// checkout at root lists it.
func NewGitRemote(name, written string, root CheckoutRoot) GitRemote {
	return GitRemote{name: name, written: written, root: root}
}

// Name is the remote's name as git spells it.
func (r GitRemote) Name() string { return r.name }

// url is the fetch URL as git reads it from the root. [LAW:one-source-of-truth]
// derived from the written URL and the root every time, never stored beside them.
func (r GitRemote) url() remoteURL { return readRemoteURL(r.written, r.root) }

// SyncableRemote is the one parse of a git remote into a remote lit syncs
// with: a name Dolt can hold and a URL that means one repository wherever it
// is read. Either refusal is a RefusedGitRemote, which unwraps to a
// storage.RefusedRemoteError, so every caller that tells a refused remote from
// a failure asks one question. [LAW:single-enforcer]
func (r GitRemote) SyncableRemote() (SyncableRemote, error) {
	url := r.url()
	name, err := storage.ParseRemoteName(r.name)
	if refusal := (storage.RefusedRemoteError{}); errors.As(err, &refusal) {
		return SyncableRemote{}, RefusedGitRemote{Remote: r, Reason: refusal.Reason}
	}
	if err != nil {
		return SyncableRemote{}, err
	}
	if url.refusal != nil {
		return SyncableRemote{}, RefusedGitRemote{Remote: r, Reason: url.refusal}
	}
	return SyncableRemote{name: name, url: url}, nil
}

// RefusedGitRemote is a remote lit refuses to sync with, carried whole, so a
// caller that still has a question for it (init's fork check) asks the remote
// that was refused rather than looking it up again.
type RefusedGitRemote struct {
	Remote GitRemote
	Reason storage.RemoteRefusal
}

// Refusal is the refusal as storage names it. [LAW:one-source-of-truth] its
// name is the remote's, derived here, never stored beside it.
func (e RefusedGitRemote) Refusal() storage.RefusedRemoteError {
	return storage.RefusedRemoteError{Name: e.Remote.name, Reason: e.Reason}
}

func (e RefusedGitRemote) Error() string { return e.Refusal().Error() }

func (e RefusedGitRemote) Unwrap() error { return e.Refusal() }

// SyncableRemote is a git remote lit syncs with. Only GitRemote.SyncableRemote
// builds one that names a remote; the zero value names none.
type SyncableRemote struct {
	name storage.RemoteName
	url  remoteURL
}

func (s SyncableRemote) Name() storage.RemoteName { return s.name }

// URL is the remote's fetch URL as git's transport reads it: the URL as
// written, or a relative path made absolute against the checkout root.
func (s SyncableRemote) URL() string { return s.url.location }

// Joined reports whether URL is a relative path made absolute.
func (s SyncableRemote) Joined() bool { return s.url.joined }

// remoteURL is a fetch URL as git reads it. location is what git's transport
// reads; refusal is set instead when no value lit can hand Dolt names the
// repository git names.
type remoteURL struct {
	location string
	joined   bool
	refusal  storage.RemoteRefusal
}

// readRemoteURL reads a fetch URL the way git does for a command run at root.
// A relative local path is joined to root, which is what git does for lit's
// invocations, since they run at the root; every other URL is read as
// written. A path whose meaning a join cannot give, and a URL whose edges
// Dolt would trim, are refused.
func readRemoteURL(written string, root CheckoutRoot) remoteURL {
	refused := func(r storage.RemoteRefusal) remoteURL { return remoteURL{refusal: r} }
	if written == "" || strings.TrimSpace(written) != written {
		return refused(storage.SurroundingWhitespace{})
	}
	if !isLocalPath(written) {
		return remoteURL{location: written}
	}
	volume := filepath.VolumeName(written)
	switch {
	case volume != "" && !filepath.IsAbs(written):
		return refused(storage.DriveRelativePath{URL: written})
	case strings.HasPrefix(written, "~"):
		return refused(storage.HomeRelativePath{URL: written})
	case climbsAfterName(written[len(volume):]):
		return refused(storage.PathThroughFilesystem{URL: written})
	case filepath.IsAbs(written) || isPathSeparator(rune(written[0])):
		// A leading separator is rooted even where IsAbs says otherwise: on
		// Windows `/srv/x.git` has no volume (FORKS.md, Patch 12).
		return remoteURL{location: written}
	}
	if root.path == "" {
		panic(fmt.Sprintf("workspace: remote URL %q joined to the zero CheckoutRoot; build the root from git's --show-toplevel or ParseCheckoutRoot", written))
	}
	return remoteURL{location: filepath.Join(root.path, written), joined: true}
}

// isLocalPath is git's choice of a local-path transport: git's transport_get
// (transport.c) takes `<transport>::<address>` and `scheme://` first, then
// url_is_local_not_ssh (connect.c) reads a URL as local when it has no colon,
// has a "/" before its first colon, or, on Windows, starts with a drive
// letter. Every other URL is scp-like ssh.
func isLocalPath(url string) bool {
	if isTransportHelper(url) || isURL(url) {
		return false
	}
	colon := strings.IndexByte(url, ':')
	slash := strings.IndexByte(url, '/')
	return colon < 0 || (slash >= 0 && slash < colon) || hasDOSDrivePrefix(url)
}

// isURLSchemeChar is git's is_urlschemechar (url.c).
func isURLSchemeChar(first bool, c byte) bool {
	alpha := ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	if first {
		return alpha
	}
	return alpha || ('0' <= c && c <= '9') || c == '+' || c == '.' || c == '-'
}

// isTransportHelper is transport_get's `<transport>::<address>` test.
func isTransportHelper(url string) bool {
	i := 0
	for i < len(url) && isURLSchemeChar(i == 0, url[i]) {
		i++
	}
	return i > 0 && strings.HasPrefix(url[i:], "::")
}

// isURL is git's is_url (url.c): a scheme, then "://".
func isURL(url string) bool {
	if url == "" || !isURLSchemeChar(true, url[0]) {
		return false
	}
	i := 1
	for i < len(url) && url[i] != ':' {
		if !isURLSchemeChar(false, url[i]) {
			return false
		}
		i++
	}
	return strings.HasPrefix(url[i:], "://")
}

// hasDOSDrivePrefix is git's has_dos_drive_prefix, which only Windows git has.
func hasDOSDrivePrefix(url string) bool {
	return runtime.GOOS == "windows" && len(url) >= 2 && isURLSchemeChar(true, url[0]) && url[1] == ':'
}

// climbsAfterName reports whether a path has a ".." after a directory name.
// The OS resolves a symlink before the "..", while a lexical join, and Dolt's
// filepath.Abs, which cleans an absolute path, drop the name first; only a
// ".." that follows no name means the same either way.
func climbsAfterName(path string) bool {
	named := false
	for _, segment := range strings.FieldsFunc(path, isPathSeparator) {
		switch segment {
		case ".":
		case "..":
			if named {
				return true
			}
		default:
			named = true
		}
	}
	return false
}

func isPathSeparator(r rune) bool {
	return r < utf8.RuneSelf && os.IsPathSeparator(uint8(r))
}

// remoteListing is git's remote list as `git remote -v` prints it: the remotes
// with a fetch URL, sorted by name, and the names of remotes with none.
type remoteListing struct {
	remotes    []GitRemote
	withoutURL []string
}

// gitRemotes reads the remote list of the checkout at root. The output is read
// untrimmed: a URL can end in whitespace, and a remote with no fetch URL is a
// line that ends in its tab, which a trim of the whole output would cut when
// that line is the last.
func gitRemotes(ctx context.Context, env environ.Env, root CheckoutRoot) (remoteListing, error) {
	out, err := gitStdout(ctx, env, root.path, "remote", "-v")
	if err != nil {
		return remoteListing{}, fmt.Errorf("git remote -v in %q: %w", root.path, err)
	}
	return parseRemoteListing(out, root)
}

// parseRemoteListing reads each line of `git remote -v` by its shape:
//
//	name TAB url SP "(fetch)"            the remote's fetch URL, spaces and all
//	name TAB url SP "(fetch)" SP [filter] the same, for a partial clone
//	name TAB url SP "(push)"             a push URL, which is not lit's
//	name TAB                             a remote with no fetch URL
//
// Any other line is an error naming it. [LAW:parse-dont-validate]
func parseRemoteListing(out string, root CheckoutRoot) (remoteListing, error) {
	fetchURL := map[string]string{}
	urlless := map[string]bool{}
	if out != "" {
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			name, rest, ok := strings.Cut(line, "\t")
			if !ok || name == "" {
				return remoteListing{}, fmt.Errorf("git remote -v printed %q, which is no remote line", line)
			}
			if rest == "" {
				urlless[name] = true
				continue
			}
			if url, ok := cutFetchScope(rest); ok {
				fetchURL[name] = url
				continue
			}
			if _, ok := strings.CutSuffix(rest, " (push)"); !ok {
				return remoteListing{}, fmt.Errorf("git remote -v printed %q, which is no remote line", line)
			}
		}
	}
	listing := remoteListing{remotes: []GitRemote{}, withoutURL: []string{}}
	for _, name := range slices.Sorted(maps.Keys(fetchURL)) {
		listing.remotes = append(listing.remotes, NewGitRemote(name, fetchURL[name], root))
	}
	for _, name := range slices.Sorted(maps.Keys(urlless)) {
		if _, fetched := fetchURL[name]; !fetched {
			listing.withoutURL = append(listing.withoutURL, name)
		}
	}
	return listing, nil
}

// cutFetchScope cuts the " (fetch)" scope off a fetch line's URL, and the
// " [filter]" git prints after it for a partial clone's remote.
func cutFetchScope(rest string) (string, bool) {
	if url, ok := strings.CutSuffix(rest, " (fetch)"); ok {
		return url, true
	}
	at := strings.LastIndex(rest, " (fetch) [")
	if at < 0 || !strings.HasSuffix(rest, "]") {
		return "", false
	}
	return rest[:at], true
}

// record sets the listing's facts on ctx's event: which relative paths became
// which absolute URLs, which remotes git lists with no fetch URL, and which
// remotes lit refuses and why. Only joined paths are named with a URL, so no
// credential a URL may carry reaches the event; each map and list is empty,
// not absent, when nothing is in it.
func (l remoteListing) record(ctx context.Context) {
	resolved := map[string]string{}
	refused := map[string]string{}
	for _, remote := range l.remotes {
		syncable, err := remote.SyncableRemote()
		var refusal storage.RefusedRemoteError
		switch {
		case errors.As(err, &refusal):
			refused[remote.name] = refusal.Reason.Kind()
		case err == nil && syncable.Joined():
			resolved[remote.name] = syncable.URL()
		}
	}
	telemetry.Set(ctx, "remotes.resolved", resolved)
	telemetry.Set(ctx, "remotes.without_url", l.withoutURL)
	telemetry.Set(ctx, "remotes.refused", refused)
}
