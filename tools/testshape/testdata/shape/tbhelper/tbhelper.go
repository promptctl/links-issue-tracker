// Package tbhelper has test helpers that take testing.TB: one only reports,
// one makes a scratch directory.
package tbhelper

import "testing"

func Check(tb testing.TB, ok bool) {
	tb.Helper()
	if !ok {
		tb.Fatalf("not ok")
	}
}

func Scratch(tb testing.TB) string {
	tb.Helper()
	return tb.TempDir()
}
