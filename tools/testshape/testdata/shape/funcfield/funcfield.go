// Package funcfield hands a function to outside code inside an options
// struct, never as an argument of its own.
package funcfield

import (
	"io"
	"log/slog"
	"os"
)

// New logs to w, redacting each attribute through a file it reads.
func New(w io.Writer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{ReplaceAttr: redact})
}

func redact(groups []string, a slog.Attr) slog.Attr {
	if _, err := os.ReadFile("redactions"); err != nil {
		return a
	}
	return slog.Attr{}
}
