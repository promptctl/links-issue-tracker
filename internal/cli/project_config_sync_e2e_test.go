package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// inForceOf is the digest `lit config status` names as in force in dir.
func inForceOf(t *testing.T, dir string) string {
	t.Helper()
	status := runCLIInDir(t, dir, "config", "status")
	line, _, _ := strings.Cut(status, "\n")
	digest, _, _ := strings.Cut(strings.TrimPrefix(line, "in force: "), ",")
	return digest
}

// The config in force travels with the store: a clone runs on the config the
// other applied, and when both apply while apart, the reconcile keeps both
// applies and each clone, once synced, runs on the one made last.
func TestEveryCloneRunsOnTheNewestApply(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")
	clone := func(name string) string {
		dir := filepath.Join(base, name)
		runGit(t, base, "clone", remote, name)
		runGit(t, dir, "config", "user.email", name+"@x.co")
		runGit(t, dir, "config", "user.name", name)
		return dir
	}

	producer := clone("alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks")
	applyProjectConfig(t, producer, requirePromptConfig)
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")
	first := model.DigestConfig(requirePromptConfig).Short()

	// The consumer's checkout carries no file of its own: what it runs on is
	// what the store carried.
	consumer := clone("bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks")
	if got := inForceOf(t, consumer); got != first {
		t.Fatalf("the consumer runs on %q, want the producer's apply %s", got, first)
	}

	// Apart, the producer applies one config and the consumer, later, another.
	const producerConfig, consumerConfig = "[quickstart]\nsoil_mode = true\n", "[ready]\nrequired_fields = [\"acceptance\"]\n"
	applyProjectConfig(t, producer, producerConfig)
	runCLIInDir(t, producer, "sync", "push")
	applyProjectConfig(t, consumer, consumerConfig)
	last := model.DigestConfig(consumerConfig).Short()

	runCLIInDir(t, consumer, "sync", "pull")
	if got := inForceOf(t, consumer); got != last {
		t.Errorf("after the reconcile the consumer runs on %q, want its own apply %s, made last", got, last)
	}
	runCLIInDir(t, consumer, "sync", "push")
	runCLIInDir(t, producer, "sync", "pull")
	if got := inForceOf(t, producer); got != last {
		t.Errorf("after syncing the producer runs on %q, want the consumer's apply %s, made last", got, last)
	}
}
