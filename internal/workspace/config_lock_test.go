package workspace

import (
	"strings"
	"sync"
	"testing"
)

// raceN runs fn from n goroutines released together, so every call reaches
// config.json's read at the same moment rather than in spawn order.
func raceN(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
}

func TestConcurrentFirstResolvesAgreeOnOneWorkspaceID(t *testing.T) {
	const racers = 16
	for round := 0; round < 10; round++ {
		repo := t.TempDir()
		run(t, repo, "git", "init")
		ids := make([]string, racers)
		errs := make([]error, racers)
		raceN(racers, func(i int) {
			info, err := Resolve(repo)
			ids[i], errs[i] = info.WorkspaceID, err
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d racer %d: Resolve() error = %v", round, i, err)
			}
		}
		info, err := Resolve(repo)
		if err != nil {
			t.Fatalf("round %d: Resolve() after the race error = %v", round, err)
		}
		// Every racer reports the id config.json holds: a racer whose own mint
		// was overwritten would report success over a value nothing records.
		for i, id := range ids {
			if id != info.WorkspaceID {
				t.Fatalf("round %d racer %d reported workspace_id %q, config.json holds %q", round, i, id, info.WorkspaceID)
			}
		}
	}
}

func TestConcurrentUpdateConfigLosesNoWrite(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init")
	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	before := info.IssuePrefix.Stored()
	const racers = 16
	errs := make([]error, racers)
	raceN(racers, func(i int) {
		_, errs[i] = UpdateConfig(info.ConfigPath, func(cfg Config) (Config, error) {
			cfg.IssuePrefix += "x"
			return cfg, nil
		})
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: UpdateConfig() error = %v", i, err)
		}
	}
	cfg, err := ReadConfig(info.ConfigPath)
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if want := string(before) + strings.Repeat("x", racers); cfg.IssuePrefix != want {
		t.Fatalf("issue_prefix = %q after %d concurrent appends, want %q", cfg.IssuePrefix, racers, want)
	}
}
