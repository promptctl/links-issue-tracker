package store

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// openFiles lists the paths of the files this process holds open: each fd
// /dev/fd names, resolved with fcntl(F_GETPATH).
func openFiles(t *testing.T) []string {
	t.Helper()
	const fdDir = "/dev/fd"
	// Names only: an lstat of each entry (os.ReadDir's) fails on the fd the
	// listing itself held, which is closed by the time it is stat'ed.
	dir, err := os.Open(fdDir)
	if err != nil {
		t.Fatalf("open %s: %v", fdDir, err)
	}
	names, err := dir.Readdirnames(-1)
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("list %s: %v", fdDir, err)
	}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		descriptor, err := strconv.Atoi(name)
		if err != nil {
			t.Fatalf("%s entry %q is not an fd", fdDir, name)
		}
		var buf [1024]byte // MAXPATHLEN, the buffer F_GETPATH fills
		switch _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); errno {
		case 0:
			// An unlinked file still open reports the path it had.
			paths = append(paths, string(buf[:bytes.IndexByte(buf[:], 0)]))
		case syscall.EBADF:
			// No file: an fd closed since the listing (the listing's own among
			// them), or one that is not a vnode, as a pipe or a socket is.
		default:
			t.Fatalf("fcntl(%d, F_GETPATH): %v", descriptor, errno)
		}
	}
	return paths
}
