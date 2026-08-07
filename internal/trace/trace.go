// Package trace is the one place a durable, timestamped JSON trace record is
// written to disk. It owns only the filename/collision mechanics — directory
// layout, unique id minting, atomic create-exclusive write, retry on
// collision — never the record shape, which stays each caller's own type.
// [LAW:one-source-of-truth] Every trace kind (automation traces today,
// workflow firing traces alongside them) shares this one writer instead of
// re-deriving the same retry loop.
package trace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nonSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// Dir returns the on-disk directory one trace kind's records live under,
// beneath a workspace's storage root: <storageDir>/traces/<kind>.
func Dir(storageDir string, kind string) string {
	return filepath.Join(storageDir, "traces", kind)
}

// Write mints a unique, timestamp-ordered id for a new trace record, lets
// build marshal the caller's record shape for that id, and writes it
// atomically under Dir(storageDir, kind). On a filename collision (two
// records minted in the same nanosecond) it retries with a fresh timestamp;
// build runs again each attempt so the record's stamped id/timestamp always
// matches the file it lands in. [LAW:no-silent-failure] every failure mode
// (mkdir, marshal, write) returns a wrapped error naming the trace kind.
func Write(storageDir string, kind string, slug string, build func(id string, recordedAt time.Time) ([]byte, error)) (id string, path string, err error) {
	dir := Dir(storageDir, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create %s trace dir: %w", kind, err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		timestamp := time.Now().UTC()
		candidate := fmt.Sprintf("%s-%s", timestamp.Format("20060102T150405.000000000Z"), slug)
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d", candidate, attempt)
		}
		payload, buildErr := build(candidate, timestamp)
		if buildErr != nil {
			return "", "", fmt.Errorf("marshal %s trace: %w", kind, buildErr)
		}
		targetPath := filepath.Join(dir, candidate+".json")
		file, openErr := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr != nil {
			if os.IsExist(openErr) {
				continue
			}
			return "", "", fmt.Errorf("create %s trace: %w", kind, openErr)
		}
		if _, writeErr := file.Write(payload); writeErr != nil {
			_ = file.Close()
			return "", "", fmt.Errorf("write %s trace: %w", kind, writeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", "", fmt.Errorf("close %s trace: %w", kind, closeErr)
		}
		return candidate, targetPath, nil
	}
	return "", "", fmt.Errorf("create %s trace: too many id collisions", kind)
}

// Slug canonicalizes free-form text (a trigger name, an event name) into the
// filesystem-safe token trace filenames embed after their timestamp.
// [LAW:one-source-of-truth] the one slugging rule every trace kind shares.
func Slug(input string) string {
	normalized := strings.ToLower(strings.TrimSpace(input))
	normalized = nonSlugPattern.ReplaceAllString(normalized, "-")
	normalized = strings.Trim(normalized, "-")
	if normalized == "" {
		return "trace"
	}
	return normalized
}
