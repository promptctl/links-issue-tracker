package syncfile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	tempFile, err := os.CreateTemp(filepath.Dir(cleanPath), ".links-sync-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp sync file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(payload); err != nil {
		_ = tempFile.Close()
		return "", fmt.Errorf("write temp sync file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close temp sync file: %w", err)
	}
	if err := os.Rename(tempPath, cleanPath); err != nil {
		return "", fmt.Errorf("rename sync file: %w", err)
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

func HashFile(path string) (string, error) {
	payload, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read sync file: %w", err)
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
