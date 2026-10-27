package cli

import (
	"io"
	"strings"
	"testing"
)

// testStderr is the stderr a test hands a function it calls directly when it
// asserts nothing about that function's warnings: each line lands in the
// test's log, so a failing test still shows what was warned.
func testStderr(t testing.TB) io.Writer {
	return testLogWriter{t: t}
}

type testLogWriter struct{ t testing.TB }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}
