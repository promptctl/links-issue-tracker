package syncfile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/atomicfile"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

func WriteAtomic(path string, export model.Export) (string, error) {
	payload, err := marshalExport(export)
	if err != nil {
		return "", err
	}
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o755); err != nil {
		return "", fmt.Errorf("create sync dir: %w", err)
	}
	// Durable: a backup is the copy kept for when the store is lost, so
	// nothing could recompute it.
	if err := atomicfile.WriteFile(cleanPath, payload, 0o600, atomicfile.Durable); err != nil {
		return "", fmt.Errorf("write sync file: %w", err)
	}
	return hashPayload(payload), nil
}

func Read(path string) (model.Export, string, error) {
	payload, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return model.Export{}, "", fmt.Errorf("read sync file: %w", err)
	}
	var export model.Export
	if err := json.Unmarshal(payload, &export); err != nil {
		return model.Export{}, "", fmt.Errorf("parse sync file: %w", err)
	}
	return export, hashPayload(payload), nil
}

// ContentHash is the digest of what export holds, not of when it was taken:
// ExportedAt is left out, so two exports of an unchanged store hash alike.
// export must come from a store: an epic decoded from a file awaits store
// hydration and refuses to marshal.
func ContentHash(export model.Export) (string, error) {
	export.ExportedAt = time.Time{}
	payload, err := marshalExport(export)
	if err != nil {
		return "", err
	}
	return hashPayload(payload), nil
}

func marshalExport(export model.Export) ([]byte, error) {
	payload, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal export: %w", err)
	}
	return append(payload, '\n'), nil
}

func hashPayload(payload []byte) string {
	sum := sha256.Sum256(payload)
	return strings.ToLower(hex.EncodeToString(sum[:]))
}
