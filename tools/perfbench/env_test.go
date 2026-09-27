package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every variable that selects a lit code path on one machine and not another
// is removed, the config home is the run's own, and everything else — PATH,
// the cgo library paths — survives untouched.
func TestHermeticEnvStripsExactlyWhatMovesLit(t *testing.T) {
	in := []string{
		"HOME=/home/dev",
		"PATH=/usr/bin",
		"DYLD_LIBRARY_PATH=/opt/icu/lib",
		"LIT_DISABLE_AUTO_SYNC=1",
		"LIT_CONFIG_GLOBAL_PATH=/home/dev/lit.toml",
		"CLAUDE_CODE_SESSION_ID=abc",
		"XDG_CONFIG_HOME=/home/dev/.config",
		"LITERAL_NOT_LIT=keep", // LIT_ is a prefix with an underscore, not "LIT"
	}
	got := hermeticEnv(in, "/work/config-home")
	for _, want := range []string{"HOME=/home/dev", "PATH=/usr/bin", "DYLD_LIBRARY_PATH=/opt/icu/lib", "LITERAL_NOT_LIT=keep", "XDG_CONFIG_HOME=/work/config-home"} {
		if !contains(got, want) {
			t.Errorf("hermeticEnv dropped %q:\n%v", want, got)
		}
	}
	for _, kv := range got {
		key, _, _ := strings.Cut(kv, "=")
		if key == "CLAUDE_CODE_SESSION_ID" || strings.HasPrefix(key, "LIT_") {
			t.Errorf("hermeticEnv kept %q, which selects a lit code path per machine", kv)
		}
	}
	if n := count(got, "XDG_CONFIG_HOME"); n != 1 {
		t.Errorf("XDG_CONFIG_HOME appears %d times, want exactly once: with two, which one lit reads is exec's business, not this tool's", n)
	}
}

// The module root is where ./cmd/lit lives, and `go env GOMOD` finds it from
// any directory inside the module — this test's own cwd is tools/perfbench.
func TestModuleRootHoldsTheLitModule(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "lit", "main.go")); err != nil {
		t.Errorf("moduleRoot = %q, which holds no cmd/lit/main.go: %v", root, err)
	}
}

func contains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func count(env []string, key string) int {
	n := 0
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == key {
			n++
		}
	}
	return n
}
