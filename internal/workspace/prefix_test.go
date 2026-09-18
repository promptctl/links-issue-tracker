package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoNamed creates a git repository whose DIRECTORY NAME is exactly name.
// t.TempDir() names itself after the test, and the directory name is the only
// input prefix derivation reads, so a test about derivation has to own it.
// Each t.TempDir() is unique, which is what keeps "ab" and "AB" separate cases
// on a case-insensitive filesystem rather than the same case run twice.
func repoNamed(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", repo, err)
	}
	run(t, repo, "git", "init")
	return repo
}

// namesThatDeriveNothing is the two ways a repository name can fail to produce
// a prefix: too little survives normalization, or nothing does. They were once
// two error messages and two exits; the contract below is that they are one
// condition, because the act that resolves them is the same one.
var namesThatDeriveNothing = []string{"ab", "a", "AB", "Qz", "___", "...", "--"}

func TestResolveRefusesEveryNameItCannotDeriveAPrefixFrom(t *testing.T) {
	for _, name := range namesThatDeriveNothing {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(repoNamed(t, name))
			if err == nil {
				t.Fatalf("Resolve() in a repository named %q succeeded, want a refusal", name)
			}
			// The type is what the CLI dispatches on. Untyped, this reached the
			// unclassified default and told the caller to retry a deterministic
			// refusal and then run `lit doctor` on a workspace that does not exist.
			if !errors.Is(err, ErrIssuePrefixRefused) {
				t.Fatalf("Resolve() error = %v, want it to wrap ErrIssuePrefixRefused", err)
			}
			// A refusal has to name an act that works. Renaming the repository was
			// the only one before this flag existed.
			if !strings.Contains(err.Error(), "--prefix") {
				t.Fatalf("Resolve() error = %v, want it to name --prefix", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("Resolve() error = %v, want it to quote the directory name %q as typed", err, name)
			}
		})
	}
}

func TestResolveWithPrefixInitializesEveryNameDerivationRefuses(t *testing.T) {
	for _, name := range namesThatDeriveNothing {
		t.Run(name, func(t *testing.T) {
			requested, err := RequestPrefix("demo")
			if err != nil {
				t.Fatalf("RequestPrefix() error = %v", err)
			}
			repo := repoNamed(t, name)
			info, err := ResolveWithPrefix(repo, requested)
			if err != nil {
				t.Fatalf("ResolveWithPrefix() in a repository named %q error = %v", name, err)
			}
			if got := info.IssuePrefix.Value(); got != "demo" {
				t.Fatalf("IssuePrefix = %q, want demo", got)
			}
			// Provenance: the user chose this, so `lit doctor` must report it as
			// configured rather than derived. An explicit prefix that read back as
			// derived would need a third value in an enum that has two.
			if info.IssuePrefix.Derived() {
				t.Fatal("an explicitly requested prefix is configured, not derived")
			}
			// Persisted, so the next command resolves without the flag.
			var cfg Config
			payload, err := os.ReadFile(info.ConfigPath)
			if err != nil {
				t.Fatalf("ReadFile(config) error = %v", err)
			}
			if err := json.Unmarshal(payload, &cfg); err != nil {
				t.Fatalf("json.Unmarshal(config) error = %v", err)
			}
			if cfg.IssuePrefix != "demo" {
				t.Fatalf("persisted prefix = %q, want demo", cfg.IssuePrefix)
			}
			// The next command needs no flag: the prefix is on disk now, so a
			// plain Resolve of the same repository reads it back rather than
			// re-deriving and refusing.
			again, err := Resolve(repo)
			if err != nil {
				t.Fatalf("Resolve() after an explicit prefix was persisted error = %v", err)
			}
			if got := again.IssuePrefix.Value(); got != "demo" {
				t.Fatalf("IssuePrefix on reload = %q, want demo", got)
			}
		})
	}
}

func TestResolveWithPrefixLeavesADerivableNameAlone(t *testing.T) {
	// The escape hatch must not become the path: a name that derives keeps
	// deriving, and the request is still honoured where one is given.
	info, err := Resolve(repoNamed(t, "myrepo"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := info.IssuePrefix.Value(); got != "myrepo" {
		t.Fatalf("IssuePrefix = %q, want myrepo", got)
	}
	if !info.IssuePrefix.Derived() {
		t.Fatal("a prefix taken from the repository name is derived")
	}

	requested, err := RequestPrefix("demo")
	if err != nil {
		t.Fatalf("RequestPrefix() error = %v", err)
	}
	explicit, err := ResolveWithPrefix(repoNamed(t, "myrepo"), requested)
	if err != nil {
		t.Fatalf("ResolveWithPrefix() error = %v", err)
	}
	if got := explicit.IssuePrefix.Value(); got != "demo" {
		t.Fatalf("IssuePrefix = %q, want demo — an explicit prefix outranks a derivable name", got)
	}
}

func TestResolveWithPrefixRefusesToContradictAWorkspaceThatHasOne(t *testing.T) {
	repo := repoNamed(t, "myrepo")
	if _, err := Resolve(repo); err != nil {
		t.Fatalf("Resolve() initial error = %v", err)
	}

	requested, err := RequestPrefix("demo")
	if err != nil {
		t.Fatalf("RequestPrefix() error = %v", err)
	}
	_, err = ResolveWithPrefix(repo, requested)
	if err == nil {
		t.Fatal("ResolveWithPrefix() overwrote an existing workspace's prefix, want a refusal")
	}
	if !errors.Is(err, ErrIssuePrefixRefused) {
		t.Fatalf("ResolveWithPrefix() error = %v, want it to wrap ErrIssuePrefixRefused", err)
	}
	// Changing the prefix of a workspace that already has issues under it is
	// `lit prefix set`'s job, which previews the change first. The refusal has
	// to hand the caller that command, not just say no.
	if !strings.Contains(err.Error(), "lit prefix set") {
		t.Fatalf("ResolveWithPrefix() error = %v, want it to name `lit prefix set`", err)
	}

	// And nothing was written on the way to refusing.
	after, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() after refusal error = %v", err)
	}
	if got := after.IssuePrefix.Value(); got != "myrepo" {
		t.Fatalf("IssuePrefix after refusal = %q, want myrepo unchanged", got)
	}
}

func TestResolveWithPrefixAcceptsAPrefixTheWorkspaceAlreadyHas(t *testing.T) {
	// `lit init` is safe to re-run, so re-stating the prefix it already chose is
	// agreement, not contradiction. Normalization happens first, so a caller may
	// spell it differently and still be agreeing.
	repo := repoNamed(t, "myrepo")
	if _, err := Resolve(repo); err != nil {
		t.Fatalf("Resolve() initial error = %v", err)
	}
	for _, spelling := range []string{"myrepo", "MyRepo", "  myrepo  "} {
		requested, err := RequestPrefix(spelling)
		if err != nil {
			t.Fatalf("RequestPrefix(%q) error = %v", spelling, err)
		}
		info, err := ResolveWithPrefix(repo, requested)
		if err != nil {
			t.Fatalf("ResolveWithPrefix(%q) error = %v, want agreement", spelling, err)
		}
		if got := info.IssuePrefix.Value(); got != "myrepo" {
			t.Fatalf("IssuePrefix = %q, want myrepo", got)
		}
	}
}

func TestRequestPrefixMintsOnlyPresentRequests(t *testing.T) {
	// The absence is the zero value. If an empty string could mint a request,
	// `--prefix ""` would silently mean "no flag" and the caller would then be
	// told to pass the flag they just passed.
	for _, raw := range []string{"", "   ", "ab", "_"} {
		if _, err := RequestPrefix(raw); err == nil {
			t.Fatalf("RequestPrefix(%q) succeeded, want a refusal", raw)
		}
	}
	requested, err := RequestPrefix("  Demo-Service  ")
	if err != nil {
		t.Fatalf("RequestPrefix() error = %v", err)
	}
	if !requested.present {
		t.Fatal("RequestPrefix() minted an absent request")
	}
	// Normalized by the one boundary every configured prefix crosses, so the
	// workspace never re-checks what reached it.
	if got := requested.spec.Value(); got != "demo-service" {
		t.Fatalf("requested prefix = %q, want demo-service", got)
	}
	if (PrefixRequest{}).present {
		t.Fatal("the zero PrefixRequest must be the absence")
	}
}
