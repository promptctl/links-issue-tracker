package cli

import (
	"io"
	"os"

	"golang.org/x/term"
)

// interactive reports whether a person can both see what lit writes and answer
// it: stdin and stdout are each a real terminal, not a pipe, a file redirect,
// /dev/null (a character device but no terminal), or an in-memory buffer (what
// tests and any non-*os.File caller pass). Every prompt and the editor launch
// gate on it, judged on the invocation's own streams rather than the process
// globals, so a test capturing output never blocks on a read or opens an
// editor. [LAW:single-enforcer]
func interactive(stdin io.Reader, stdout io.Writer) bool {
	return isTerminal(stdin) && isTerminal(stdout)
}

// isTerminal asks through SyscallConn rather than Fd, because Fd switches a
// non-blocking file to blocking mode for the rest of the run.
func isTerminal(stream any) bool {
	file, ok := stream.(*os.File)
	if !ok {
		return false
	}
	conn, err := file.SyscallConn()
	if err != nil {
		return false
	}
	tty := false
	if err := conn.Control(func(fd uintptr) { tty = term.IsTerminal(int(fd)) }); err != nil {
		return false
	}
	return tty
}
