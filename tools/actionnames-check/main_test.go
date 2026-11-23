package main

import (
	"slices"
	"strings"
	"testing"
)

// root is the repository root, whose .goreleaser.yml names the GOOSes.
const root = "../.."

func TestShippedGOOSIsTheReleaseBuildsTargets(t *testing.T) {
	got, err := shippedGOOS(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"darwin", "linux", "windows"}; !slices.Equal(got, want) {
		t.Errorf("shippedGOOS = %q, want %q", got, want)
	}
}

func TestRunFailsNamingASiteOnlyOneGOOSBuilds(t *testing.T) {
	var stderr strings.Builder
	code := run(root, []string{"./tools/actionnames-check/testdata/bad"}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "bad_windows.go:12:50: argument to fmt.Errorf") {
		t.Errorf("exit %d, stderr:\n%s\nwant exit 1 naming bad_windows.go:12:50", code, stderr.String())
	}
}

func TestRunPassesAPackageThatFormatsOnlyVerbs(t *testing.T) {
	var stderr strings.Builder
	if code := run(root, []string{"./tools/actionnames-check/testdata/good"}, &stderr); code != 0 {
		t.Errorf("exit %d, stderr:\n%s", code, stderr.String())
	}
}
