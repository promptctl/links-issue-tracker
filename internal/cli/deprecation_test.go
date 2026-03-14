package cli

import "testing"

func TestDeprecationWarningLit(t *testing.T) {
	msg := DeprecationWarning("lit")
	if msg == "" {
		t.Fatal("expected deprecation warning for lit")
	}
}

func TestDeprecationWarningLnks(t *testing.T) {
	msg := DeprecationWarning("lnks")
	if msg != "" {
		t.Fatalf("unexpected warning for lnks: %q", msg)
	}
}
