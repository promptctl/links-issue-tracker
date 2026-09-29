package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// gitRepoNamed creates a git repository whose DIRECTORY NAME is exactly name
// and makes it the working directory. The name is the only input the prefix
// derivation reads, so a test about `lit init --prefix` has to own it; each
// t.TempDir() is unique, which keeps case-only variants from colliding on a
// case-insensitive filesystem.
func gitRepoNamed(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", repo, err)
	}
	runGit(t, repo, "init")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	return repo
}

func runInit(t *testing.T, args ...string) error {
	t.Helper()
	var out bytes.Buffer
	return Run(context.Background(), &out, &out, append([]string{"init", "--skip-hooks", "--skip-agents"}, args...))
}

// The whole contract in one test: a repository lit cannot name is unrunnable,
// and the advice printed under the failure has to name an act that works.
func TestInitInAnUnnameableRepositoryRefusesWithUsableAdvice(t *testing.T) {
	gitRepoNamed(t, "ab")

	err := runInit(t)
	if err == nil {
		t.Fatal("Run(init) in a repository named \"ab\" succeeded, want a refusal")
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Fatalf("ExitCode = %d, want %d — a self-fixable precondition is not a fault", got, ExitValidation)
	}

	var stderr bytes.Buffer
	WriteCommandError(&stderr, err)
	rendered := stderr.String()
	if strings.Contains(rendered, "lit doctor") {
		t.Fatalf("rendered error still sends the caller to `lit doctor`:\n%s", rendered)
	}
	if strings.Contains(rendered, "Retry the command") {
		t.Fatalf("rendered error still tells the caller to retry a deterministic refusal:\n%s", rendered)
	}
	if !strings.Contains(rendered, "--prefix") {
		t.Fatalf("rendered error does not name the act that works:\n%s", rendered)
	}
}

// `lit init` is not the only command that meets this. Every write path
// bootstraps a workspace when none exists, so `lit new` in a repository lit
// cannot name fails in the same place — and the escape hatch is a flag on
// `init`, which `new` does not have. That is why the refusal names the whole
// command `lit init --prefix <prefix>` rather than the bare flag: an act that
// works for one caller and not the others would be the same defect arriving by
// the back door. [LAW:single-enforcer] one message, true for every caller.
func TestABootstrappingCommandIsGivenAnActThatWorks(t *testing.T) {
	gitRepoNamed(t, "ab")

	var out bytes.Buffer
	err := Run(context.Background(), &out, &out, []string{"new", "--title", "probe", "--topic", "probe"})
	if err == nil {
		t.Fatal("Run(new) in a repository named \"ab\" succeeded, want a refusal")
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Fatalf("ExitCode = %d, want %d", got, ExitValidation)
	}

	var stderr bytes.Buffer
	WriteCommandError(&stderr, err)
	rendered := stderr.String()
	// `lit new --prefix` does not exist, so an act naming only the flag would be
	// unfollowable here.
	if !strings.Contains(rendered, "lit init --prefix") {
		t.Fatalf("rendered error does not name an act this caller can take:\n%s", rendered)
	}
	if strings.Contains(rendered, "lit doctor") || strings.Contains(rendered, "Retry the command") {
		t.Fatalf("rendered error still carries the unclassified-fault advice:\n%s", rendered)
	}
}

func TestInitAcceptsAnExplicitPrefixWhereDerivationRefuses(t *testing.T) {
	repo := gitRepoNamed(t, "ab")

	if err := runInit(t, "--prefix", "demo"); err != nil {
		t.Fatalf("Run(init --prefix demo) error = %v", err)
	}

	// The prefix is on disk, so every later command resolves without the flag.
	if err := runInit(t); err != nil {
		t.Fatalf("Run(init) after a prefix was supplied error = %v — the flag should be needed once", err)
	}
	// Asked of the package that owns store geometry rather than rebuilt here, so
	// this assertion cannot drift from where lit actually writes.
	// [LAW:one-source-of-truth]
	info, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	if got := info.IssuePrefix.Stored(); got != "demo" {
		t.Fatalf("IssuePrefix = %q, want demo", got)
	}
	config, err := os.ReadFile(info.ConfigPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if !strings.Contains(string(config), `"issue_prefix": "demo"`) {
		t.Fatalf("config = %s, want issue_prefix demo", config)
	}
}

// `--prefix ""` is a caller who typed the flag and gave it nothing. Demoting it
// to "no flag given" would derive instead, and in the repository where the flag
// is needed that lands on a message telling them to pass the flag they passed.
func TestInitRefusesAPrefixFlagWithNothingInIt(t *testing.T) {
	// Deliberately a repository whose name DOES derive. In one that does not,
	// treating `--prefix ""` as "no flag given" still fails — on the derivation
	// message, which names --prefix too — so the test would pass while the bug
	// it guards was present. Here, a demoted flag is a silent success, which is
	// the thing that must not happen.
	gitRepoNamed(t, "myrepo")

	for _, value := range []string{"", "   ", "xy", "_"} {
		err := runInit(t, "--prefix", value)
		if err == nil {
			t.Fatalf("Run(init --prefix %q) succeeded — a flag with nothing usable in it was silently ignored", value)
		}
		if got := ExitCode(err); got != ExitValidation {
			t.Fatalf("ExitCode for --prefix %q = %d, want %d", value, got, ExitValidation)
		}
		// Names the flag as the input it REFUSED, which the derivation message
		// (which also says --prefix) cannot be mistaken for.
		if !strings.Contains(err.Error(), "invalid --prefix") {
			t.Fatalf("error for --prefix %q = %v, want it to name the flag value it refused", value, err)
		}
	}
}

func TestInitUsageNamesEveryFlagItAccepts(t *testing.T) {
	gitRepoNamed(t, "myrepo")

	err := runInit(t, "unexpected-positional")
	if err == nil {
		t.Fatal("Run(init <positional>) succeeded, want a usage error")
	}
	// The usage line is the caller's map of the flag surface; a flag missing
	// from it is a flag they will not find.
	for _, flag := range []string{"--prefix", "--skip-hooks", "--skip-agents"} {
		if !strings.Contains(err.Error(), flag) {
			t.Fatalf("usage error = %v, want it to name %s", err, flag)
		}
	}
}

// rewriteStoredPrefix puts a prefix the rules refuse into a workspace's
// config.json. Reaching this state needs no bug: the docs point callers at this
// file, and a prefix legal under older length rules stays on disk after they
// tighten.
func rewriteStoredPrefix(t *testing.T, configPath string, prefix string) {
	t.Helper()
	payload, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	cfg["issue_prefix"] = prefix
	rewritten, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
}

// storedIllegalPrefixWorkspace is a workspace named `myrepo` holding one issue
// minted under myrepo, whose config.json then has its prefix set to one the
// rules refuse — the typo'd-down-to-two-characters case the repair exists for.
//
// The repository name derives cleanly on purpose: in one that cannot derive,
// every command fails with the DERIVE message, which also carries
// `issue_prefix` and would let these tests pass while measuring the wrong
// refusal entirely.
func storedIllegalPrefixWorkspace(t *testing.T) (info workspace.Info, issueID string, runLit func(args ...string) (string, error)) {
	t.Helper()
	repo := gitRepoNamed(t, "myrepo")
	if err := runInit(t); err != nil {
		t.Fatalf("Run(init) in a nameable repository error = %v", err)
	}
	runLit = func(args ...string) (string, error) {
		var out bytes.Buffer
		err := Run(context.Background(), &out, &out, args)
		return out.String(), err
	}
	out, err := runLit("new", "--title", "filed before the typo", "--topic", "probe")
	if err != nil {
		t.Fatalf("Run(new) error = %v\n%s", err, out)
	}
	issueID = issueIDFromNew(out)
	info, err = workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	rewriteStoredPrefix(t, info.ConfigPath, "ab")
	return info, issueID, runLit
}

// Minting is what a stored illegal prefix refuses, with its own reason and a
// remediation naming the command that repairs it.
func TestAStoredIllegalPrefixRefusesMintingAndNamesTheRepair(t *testing.T) {
	info, _, runLit := storedIllegalPrefixWorkspace(t)

	_, err := runLit("new", "--title", "probe", "--topic", "probe")
	if err == nil {
		t.Fatal("Run(new) over a stored illegal prefix succeeded, want a refusal")
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Fatalf("ExitCode = %d, want %d — a repairable config is not a fault", got, ExitValidation)
	}
	// Its own reason, not the family's: the two command-fixable prefix refusals
	// are cleared by adjusting the command, and this one is not, so it cannot
	// inherit a remediation that says so.
	if got := commandErrorReason(err); got != "stored_prefix_refused" {
		t.Fatalf("reason = %q, want stored_prefix_refused", got)
	}

	var stderr bytes.Buffer
	WriteCommandError(&stderr, err)
	rendered := stderr.String()
	if strings.Contains(rendered, "Retry the command") || strings.Contains(rendered, "adjust the command") {
		t.Fatalf("rendered error tells the caller to retry or adjust a command that was never the problem:\n%s", rendered)
	}
	if !strings.Contains(rendered, "lit prefix set") {
		t.Fatalf("rendered error does not name the repair command:\n%s", rendered)
	}
	if !strings.Contains(rendered, info.ConfigPath) || !strings.Contains(rendered, `"ab"`) {
		t.Fatalf("rendered error does not name the file and the value it refuses:\n%s", rendered)
	}
}

// The whole repair, in the order an operator meets it: nothing but minting is
// refused, doctor reports the state and exits on it, init --prefix will not
// overwrite it, and `lit prefix set` previews against the ids the store holds
// and then repairs it.
// [LAW:no-silent-failure] the remediation above has to name an act that works.
func TestPrefixSetRepairsAStoredIllegalPrefix(t *testing.T) {
	info, issueID, runLit := storedIllegalPrefixWorkspace(t)

	// Reading needs no prefix to mint under, so it is not refused.
	if out, err := runLit("show", issueID); err != nil {
		t.Fatalf("Run(show) over a stored illegal prefix error = %v\n%s", err, out)
	}

	out, err := runLit("doctor")
	if got := commandErrorReason(err); got != "stored_prefix_refused" {
		t.Fatalf("doctor reason = %q (err %v), want stored_prefix_refused — a workspace that cannot mint is not healthy", got, err)
	}
	for _, want := range []string{
		"issue_prefix=ab ",
		"id_prefixes=myrepo:1 ",
		`prefix: issue_prefix "ab" matches none of this store's issue ids (myrepo:1) — run 'lit prefix set myrepo'`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}

	// init --prefix is not a second door onto the repair: it cannot see the ids.
	out, err = runLit("init", "--skip-hooks", "--skip-agents", "--prefix", "myrepo")
	if err == nil || !strings.Contains(err.Error(), "lit prefix set myrepo") {
		t.Fatalf("Run(init --prefix) error = %v, want the refusal naming `lit prefix set myrepo`\n%s", err, out)
	}

	out, err = runLit("prefix", "set", "myrepo")
	if err != nil {
		t.Fatalf("Run(prefix set) preview error = %v\n%s", err, out)
	}
	if !strings.Contains(out, "issue_prefix: ab -> myrepo (preview)") || !strings.Contains(out, "issue ids in this store use: myrepo:1") {
		t.Fatalf("preview does not show both the stored prefix and what the ids use:\n%s", out)
	}
	cfg, err := workspace.ReadConfig(info.ConfigPath)
	if err != nil || cfg.IssuePrefix != "ab" {
		t.Fatalf("config issue_prefix after preview = %q (err %v), want ab untouched", cfg.IssuePrefix, err)
	}

	if out, err := runLit("prefix", "set", "myrepo", "--apply"); err != nil {
		t.Fatalf("Run(prefix set --apply) error = %v\n%s", err, out)
	}
	out, err = runLit("new", "--title", "after the repair", "--topic", "probe")
	if err != nil {
		t.Fatalf("Run(new) after the repair error = %v\n%s", err, out)
	}
	if id := issueIDFromNew(out); !strings.HasPrefix(id, "myrepo-probe-") {
		t.Fatalf("new id after the repair = %q, want it minted under myrepo", id)
	}
	if out, err := runLit("doctor"); err != nil || strings.Contains(out, "prefix: ") {
		t.Fatalf("doctor after the repair error = %v, want a clean run with no prefix line\n%s", err, out)
	}
}

// An explicit prefix is NORMALIZED on the way in — slugified, and truncated at
// PrefixMaxLength — so the prefix lit stores is not always the prefix the caller
// typed. Reporting it is the difference between the caller learning the real
// value here and learning it from the first issue id. `lit prefix set` has
// always echoed its normalized result; init echoing it too is the two siblings
// telling one truth the same way. [LAW:no-silent-failure]
//
// `payment_service` is chosen because it exercises BOTH transformations at once:
// the underscore slugifies to a dash, and `payment-service` is 15 characters, so
// it truncates to 12. A value that survived normalization unchanged would let
// this test pass without the echo being correct.
func TestInitReportsThePrefixItActuallyStored(t *testing.T) {
	gitRepoNamed(t, "myrepo")

	var out bytes.Buffer
	err := Run(context.Background(), &out, &out,
		[]string{"init", "--skip-hooks", "--skip-agents", "--prefix", "payment_service"})
	if err != nil {
		t.Fatalf("Run(init --prefix payment_service) error = %v\n%s", err, out.String())
	}

	const stored = "payment-serv"
	if !strings.Contains(out.String(), stored) {
		t.Fatalf("init output never names the prefix it stored (%q):\n%s", stored, out.String())
	}
	// The typed value and the printed one are the same fact; if they can drift,
	// the report is decoration. [LAW:one-source-of-truth]
	if !strings.Contains(out.String(), "issue_prefix: "+stored) {
		t.Fatalf("init output does not report issue_prefix in the shape `lit prefix set` uses:\n%s", out.String())
	}
	// And what it printed is what a later command actually reads back.
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	info, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := info.IssuePrefix.Stored(); got != stored {
		t.Fatalf("stored prefix = %q, want %q — init reported a value the workspace does not carry", got, stored)
	}
}

// A refused command line must leave nothing behind. The pipeline acquires the
// workspace BEFORE it runs a leaf's work, and acquiring resolves the workspace,
// which writes config.json — so an arity check living inside work() runs only
// after the prefix is already on disk. The explicit-prefix case is the one that
// bites: a value the caller typed, in the very command line that also carried
// the bad argument, outlives a command that reported failure, and clearing it
// needs `lit prefix set` rather than a corrected re-run.
//
// The derived case is here because it carries no `--prefix`, so it shows the
// guarantee is about ORDERING rather than about the flag.
//
// Asserting the exit code alone would pass even when the refusal is correct but
// arrives after the write. The assertion that earns its keep is the ABSENCE of
// config.json. [LAW:effects-at-boundaries]
func TestAFailedInitLeavesNoWorkspaceBehind(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"an explicit prefix is not persisted by a command that fails", []string{"--prefix", "demo", "stray"}},
		{"a derived prefix is not persisted either", []string{"stray"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := gitRepoNamed(t, "myrepo")

			err := runInit(t, testCase.args...)
			if err == nil {
				t.Fatalf("Run(init %v) = nil, want a usage refusal", testCase.args)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("ExitCode(%v) = %d, want ExitUsage (%d)", err, got, ExitUsage)
			}

			configPath := filepath.Join(repo, ".git", "links", "config.json")
			if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
				t.Fatalf("a refused `lit init %v` left %s on disk; the caller's prefix outlives a command that reported failure", testCase.args, configPath)
			}
		})
	}
}
