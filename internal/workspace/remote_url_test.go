package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// How git reads a fetch URL, as lit hands it on: as written, joined to the
// checkout root, or refused with a reason. One row per shape, each with what
// git does on POSIX and on Windows (the Windows column runs on the nightly
// windows lane).
func TestARemoteURLIsReadAsGitReadsIt(t *testing.T) {
	const asWritten, joined = "as written", "joined"
	const (
		throughFilesystem = "path_through_filesystem"
		driveRelative     = "drive_relative_path"
		homeRelative      = "home_relative_path"
		whitespace        = "surrounding_whitespace"
	)
	root := testRoot(t, t.TempDir())
	for _, tc := range []struct {
		written        string
		posix, windows string
	}{
		{"https://h/x.git", asWritten, asWritten},
		{"ssh://h/x.git", asWritten, asWritten},
		{"file:///srv/x.git", asWritten, asWritten},
		{"C://x", asWritten, asWritten},
		{"ext::ssh h %S x", asWritten, asWritten},
		{"C::x", asWritten, asWritten},
		{"git@h:x.git", asWritten, asWritten},
		{"h:path/x.git", asWritten, asWritten},
		{"a:b/c.git", asWritten, asWritten},
		{"C:/x.git", asWritten, asWritten},
		{`C:\x.git`, asWritten, asWritten},
		{"C:x.git", asWritten, driveRelative},
		{"/srv/x.git", asWritten, asWritten},
		{`\srv\x.git`, joined, asWritten},
		{`\\host\share\x.git`, joined, asWritten},
		{"./x.git", joined, joined},
		{"../x.git", joined, joined},
		{"../../x.git", joined, joined},
		{"./../x.git", joined, joined},
		{`..\x.git`, joined, joined},
		{"remote.git", joined, joined},
		{"sub/remote.git", joined, joined},
		{"sub/./remote.git", joined, joined},
		{"./a:b.git", joined, joined},
		{"sub/a:b.git", joined, joined},
		{"../my remote.git", joined, joined},
		{"./~/x.git", joined, joined},
		{"link/../remote.git", throughFilesystem, throughFilesystem},
		{"../a/../x.git", throughFilesystem, throughFilesystem},
		{`a\..\x.git`, joined, throughFilesystem},
		{"/srv/link/../x.git", throughFilesystem, throughFilesystem},
		{"/srv/../x.git", throughFilesystem, throughFilesystem},
		{"/../x.git", asWritten, asWritten},
		{`C:\a\..\x.git`, asWritten, throughFilesystem},
		{`\\host\share\a\..\x.git`, joined, throughFilesystem},
		{"~/x.git", homeRelative, homeRelative},
		{"~user/x.git", homeRelative, homeRelative},
		{"", whitespace, whitespace},
		{" ", whitespace, whitespace},
		{"../x.git ", whitespace, whitespace},
		{" https://h/x.git", whitespace, whitespace},
		{"../x.git\t", whitespace, whitespace},
	} {
		want := tc.posix
		if runtime.GOOS == "windows" {
			want = tc.windows
		}
		got := readRemoteURL(tc.written, root)
		switch want {
		case asWritten:
			if got != (remoteURL{location: tc.written}) {
				t.Errorf("readRemoteURL(%q) = %+v, want it as written", tc.written, got)
			}
		case joined:
			if got != (remoteURL{location: filepath.Join(root.path, tc.written), joined: true}) {
				t.Errorf("readRemoteURL(%q) = %+v, want it joined to %q", tc.written, got, root.path)
			}
		default:
			if got.refusal == nil || got.refusal.Kind() != want {
				t.Errorf("readRemoteURL(%q) = %+v, want refused: %s", tc.written, got, want)
			}
		}
	}
}

// A join over the zero CheckoutRoot is a programmer error, never a relative
// path in a value whose type rules one out; a URL that joins nothing needs no
// root.
func TestAJoinOverTheZeroCheckoutRootPanics(t *testing.T) {
	if got := readRemoteURL("https://h/x.git", CheckoutRoot{}); got.location != "https://h/x.git" {
		t.Fatalf("readRemoteURL(https) over the zero root = %+v, want it as written", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("readRemoteURL(../x.git) over the zero root did not panic")
		}
	}()
	readRemoteURL("../x.git", CheckoutRoot{})
}

// A URL that is not a path can carry a credential, and a refusal's message
// reaches events and traces, so a refused one is never quoted.
func TestARefusedURLWithACredentialKeepsItOutOfTheRefusal(t *testing.T) {
	remote := NewGitRemote("origin", "https://u:ghp_secret@h/x.git ", CheckoutRoot{})
	_, err := remote.SyncableRemote()
	var refusal storage.RefusedRemoteError
	if !errors.As(err, &refusal) || refusal.Reason.Kind() != "surrounding_whitespace" {
		t.Fatalf("SyncableRemote() error = %v, want a surrounding_whitespace refusal", err)
	}
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("refusal %q quotes the credential", err)
	}
}

func TestParseCheckoutRootRefusesARootAJoinCannotTrust(t *testing.T) {
	resolved := testRoot(t, t.TempDir()).path
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, path := range []string{"", filepath.Join("rel", "root"), link} {
		if root, err := ParseCheckoutRoot(path); err == nil {
			t.Errorf("ParseCheckoutRoot(%q) = %q, want a refusal", path, root)
		}
	}
	if root, err := ParseCheckoutRoot(resolved); err != nil || root.path != resolved {
		t.Fatalf("ParseCheckoutRoot(%q) = %q, %v; want it as it stands", resolved, root, err)
	}
}

// `git remote -v` has three line shapes, and a remote with no fetch URL is a
// line that ends in its tab — the last line, when that remote sorts last.
func TestRemoteListingReadsEveryLineShapeGitPrints(t *testing.T) {
	root := testRoot(t, t.TempDir())
	out := "origin\thttps://h/o.git (fetch)\n" +
		"origin\thttps://h/o.git (push)\n" +
		"pushonly\t\n" +
		"pushonly\thttps://h/p.git (push)\n" +
		"spaced\t../my remote.git (fetch)\n" +
		"spaced\t../my remote.git (push)\n" +
		"thin\thttps://h/t.git (fetch) [blob:none]\n" +
		"thin\thttps://h/t.git (push)\n" +
		"zzz-empty\t\n"
	listing, err := parseRemoteListing(out, root)
	if err != nil {
		t.Fatalf("parseRemoteListing() error = %v", err)
	}
	var names []string
	for _, remote := range listing.remotes {
		names = append(names, remote.Name())
	}
	if !reflect.DeepEqual(names, []string{"origin", "spaced", "thin"}) {
		t.Fatalf("remotes = %v, want [origin spaced thin]", names)
	}
	if got := listing.remotes[2].url.location; got != "https://h/t.git" {
		t.Fatalf("partial clone's URL = %q, want it without the filter", got)
	}
	if got := listing.remotes[1].url.location; got != filepath.Join(root.path, "../my remote.git") {
		t.Fatalf("spaced URL = %q, want the whole path joined", got)
	}
	if !reflect.DeepEqual(listing.withoutURL, []string{"pushonly", "zzz-empty"}) {
		t.Fatalf("withoutURL = %v, want [pushonly zzz-empty]", listing.withoutURL)
	}

	if empty, err := parseRemoteListing("", root); err != nil || len(empty.remotes) != 0 || len(empty.withoutURL) != 0 {
		t.Fatalf("parseRemoteListing(\"\") = %+v, %v; want no remotes", empty, err)
	}
	for _, line := range []string{
		"origin https://h/o.git (fetch)\n",
		"\thttps://h/o.git (fetch)\n",
		"origin\thttps://h/o.git\n",
	} {
		if _, err := parseRemoteListing(line, root); err == nil {
			t.Errorf("parseRemoteListing(%q) succeeded, want the line named as no remote line", line)
		}
	}
}

// git resolves a relative remote URL against the top of the checkout it runs
// in, so one `../remote.git` names a different repository from a primary
// checkout and from a linked worktree at a different depth, and lit reads the
// two values git does.
func TestARelativeRemoteURLIsMadeAbsoluteAgainstTheCheckoutThatReadsIt(t *testing.T) {
	repo := litRepoWithCommit(t)
	run(t, repo, "git", "remote", "add", "origin", "../remote.git")
	worktree := filepath.Join(repo, "deep", "wt2")
	run(t, repo, "git", "worktree", "add", "-b", "wt2", worktree)

	urlFrom := func(dir string) string {
		t.Helper()
		info, err := Resolve(testEnv(), dir)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", dir, err)
		}
		remotes, err := info.Remotes.List(eventContext())
		if err != nil || len(remotes) != 1 {
			t.Fatalf("Remotes.List() from %q = %v, %v; want origin", dir, remotes, err)
		}
		syncable, err := remotes[0].SyncableRemote()
		if err != nil {
			t.Fatalf("SyncableRemote() from %q error = %v", dir, err)
		}
		return syncable.URL()
	}
	fromPrimary, fromWorktree := urlFrom(filepath.Join(repo)), urlFrom(worktree)
	resolvedRepo := testRoot(t, repo).path
	if want := filepath.Join(filepath.Dir(resolvedRepo), "remote.git"); fromPrimary != want {
		t.Errorf("origin from the primary checkout = %q, want %q", fromPrimary, want)
	}
	if want := filepath.Join(resolvedRepo, "deep", "remote.git"); fromWorktree != want {
		t.Errorf("origin from the linked worktree = %q, want %q", fromWorktree, want)
	}
}

// The read every remote consumer passes through records, once, on the first
// reader's event: which relative paths became which URLs, which remotes have
// no fetch URL, and which lit refuses and why.
func TestReadingTheRemotesRecordsWhatTheReadDecided(t *testing.T) {
	repo := litRepoWithCommit(t)
	run(t, repo, "git", "remote", "add", "origin", "https://h/o.git")
	run(t, repo, "git", "remote", "add", "relative", "../remote.git")
	run(t, repo, "git", "remote", "add", "linked", "link/../remote.git")
	run(t, repo, "git", "remote", "add", "a.b", "https://h/ab.git")
	run(t, repo, "git", "config", "remote.pushonly.pushurl", "https://h/p.git")
	run(t, repo, "git", "config", "remote.zzz-empty.url", "")

	recorder := telemetry.Begin(telemetry.Event{})
	remotes := NewRemotes(testEnv(), testRoot(t, repo))
	list, err := remotes.List(telemetry.With(context.Background(), recorder))
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("List() = %v, want the four remotes with a fetch URL", list)
	}
	later := telemetry.Begin(telemetry.Event{})
	if _, err := remotes.List(telemetry.With(context.Background(), later)); err != nil {
		t.Fatalf("a later List() error = %v", err)
	}

	facts := recorder.Finish(telemetry.End{}).Facts
	want := map[string]any{
		"remotes.resolved":    map[string]string{"relative": filepath.Join(filepath.Dir(testRoot(t, repo).path), "remote.git")},
		"remotes.without_url": []string{"pushonly", "zzz-empty"},
		"remotes.refused":     map[string]string{"a.b": "name_character", "linked": "path_through_filesystem"},
	}
	for key, value := range want {
		if !reflect.DeepEqual(facts[key], value) {
			t.Errorf("%s = %#v, want %#v", key, facts[key], value)
		}
	}
	if laterFacts := later.Finish(telemetry.End{}).Facts; len(laterFacts) != 0 {
		t.Errorf("a later reader's event = %#v, want nothing: the read records once", laterFacts)
	}

	var refusal storage.RefusedRemoteError
	for _, remote := range list {
		if remote.Name() == "linked" {
			if _, err := remote.SyncableRemote(); !errors.As(err, &refusal) || refusal.Name != "linked" {
				t.Fatalf("SyncableRemote(linked) error = %v, want a RefusedRemoteError naming linked", err)
			}
		}
	}
}
