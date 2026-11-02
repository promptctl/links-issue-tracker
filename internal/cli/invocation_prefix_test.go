package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func resolveForTest(t *testing.T, repo string) workspace.Info {
	t.Helper()
	info, err := workspace.Resolve(testEnv(), repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	return info
}

// What resolving the workspace did to config.json is on the event: the first
// init created it, a later command read it, and one that found a prefix the
// rules write differently rewrote it. Each says how long it queued for the
// lock, and which arm of PrefixState the prefix took.
func TestResolvingTheWorkspaceSaysWhatItDidToConfig(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	settle := func(want string) map[string]any {
		t.Helper()
		facts := lastEvent(t).Facts
		if facts["workspace.config_settle"] != want {
			t.Errorf("workspace.config_settle = %#v, want %q", facts["workspace.config_settle"], want)
		}
		if _, ok := facts["workspace.config_lock_wait_ms"].(float64); !ok {
			t.Errorf("workspace.config_lock_wait_ms = %#v, want a number", facts["workspace.config_lock_wait_ms"])
		}
		return facts
	}
	facts := settle("created")
	if facts["workspace.prefix_state"] != "legal" || facts["workspace.prefix_derived"] != true {
		t.Errorf("lit init prefix facts = %v / %v, want legal and derived", facts["workspace.prefix_state"], facts["workspace.prefix_derived"])
	}

	runCLIInDir(t, repo, "workspace")
	facts = settle("read")
	if facts["workspace.prefix_derived"] != false {
		t.Errorf("workspace.prefix_derived = %#v on a prefix read from config, want false", facts["workspace.prefix_derived"])
	}

	info := resolveForTest(t, repo)
	rewriteStoredPrefix(t, info.ConfigPath, "  "+string(info.IssuePrefix.Stored())+"  ")
	runCLIInDir(t, repo, "workspace")
	settle("renormalized")
}

// A stored prefix the rules refuse puts the workspace in the refused arm, and a
// command that needed to mint says it was refused and why; one that minted
// says it was not.
func TestAMintRefusalIsOnTheEvent(t *testing.T) {
	isolateEvents(t)
	_, _, runLit := storedIllegalPrefixWorkspace(t)
	if _, err := runLit("new", "--type", "epic", "--title", "probe", "--topic", "probe"); err == nil {
		t.Fatal("Run(new) over a stored illegal prefix succeeded, want a refusal")
	}
	facts := lastEvent(t).Facts
	if facts["workspace.prefix_state"] != "refused" || facts["prefix.mintable_refused"] != true {
		t.Errorf("facts = %v, want prefix_state refused and mintable_refused true", facts)
	}
	if text, _ := facts["prefix.mintable_error"].(string); text == "" {
		t.Errorf("prefix.mintable_error = %#v, want the refusal", facts["prefix.mintable_error"])
	}

	isolateEvents(t)
	repo := newInitTestRepo(t)
	newEpicInDir(t, repo, "minted")
	facts = lastEvent(t).Facts
	if facts["prefix.mintable_refused"] != false || facts["prefix.mintable_error"] != nil {
		t.Errorf("lit new facts = %v / %v, want not refused and no error", facts["prefix.mintable_refused"], facts["prefix.mintable_error"])
	}
}

// doctor and prefix set put the id-prefix census they took on the event, and
// prefix set --apply says how long it queued for the config lock.
func TestTheIDPrefixCensusIsOnTheEvent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	newEpicInDir(t, repo, "counted")
	prefix := string(resolveForTest(t, repo).IssuePrefix.Stored())
	for _, args := range [][]string{{"doctor"}, {"prefix", "set", "other", "--apply"}} {
		_, _ = runCLIInDirAllowError(t, repo, args...)
		facts := lastEvent(t).Facts
		counts, ok := facts["prefix_census.counts"].(map[string]any)
		if !ok || counts[prefix] != float64(1) {
			t.Errorf("%v prefix_census.counts = %#v, want %s:1", args, facts["prefix_census.counts"], prefix)
		}
		if facts["prefix_census.unreadable"] != float64(0) || facts["prefix_census.no_store"] != false {
			t.Errorf("%v census facts = %v / %v, want 0 unreadable and a store", args, facts["prefix_census.unreadable"], facts["prefix_census.no_store"])
		}
	}
	if _, ok := lastEvent(t).Facts["prefix_set.config_lock_wait_ms"].(float64); !ok {
		t.Errorf("prefix set --apply facts = %v, want prefix_set.config_lock_wait_ms", lastEvent(t).Facts)
	}
}
