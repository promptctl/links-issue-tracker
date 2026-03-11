package doltcli

import (
	"errors"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	version, err := ParseVersion("dolt version 1.83.4")
	if err != nil {
		t.Fatalf("ParseVersion() error = %v", err)
	}
	if version.Major != 1 || version.Minor != 83 || version.Patch != 4 {
		t.Fatalf("version = %#v", version)
	}
}

func TestVersionLessThan(t *testing.T) {
	t.Parallel()
	if !(Version{Major: 1, Minor: 81, Patch: 9}).LessThan(Version{Major: 1, Minor: 81, Patch: 10}) {
		t.Fatal("expected lower patch to be less")
	}
	if (Version{Major: 1, Minor: 81, Patch: 10}).LessThan(Version{Major: 1, Minor: 81, Patch: 10}) {
		t.Fatal("equal versions should not be less")
	}
	if (Version{Major: 1, Minor: 82, Patch: 0}).LessThan(Version{Major: 1, Minor: 81, Patch: 10}) {
		t.Fatal("higher minor should not be less")
	}
}

func TestIsManifestReadOnlyError(t *testing.T) {
	t.Parallel()
	err := errors.New("Error 1105: cannot update manifest: database is read only")
	if !IsManifestReadOnlyError(err) {
		t.Fatal("expected manifest read-only error to match")
	}
}

func TestIsManifestReadOnlyErrorReturnsFalseForOtherErrors(t *testing.T) {
	t.Parallel()
	if IsManifestReadOnlyError(errors.New("permission denied")) {
		t.Fatal("expected non-manifest error to not match")
	}
}
