package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type localStorage struct {
	basePath string
}

func NewLocalStorage(basePath string) (*localStorage, error) {
	abs, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path: %w", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}
	return &localStorage{basePath: abs}, nil
}

// resolveContained returns the absolute path corresponding to `key` and refuses
// any key that would resolve outside basePath. Belt-and-suspenders against
// path traversal in callers that build storage keys from user input.
func (l *localStorage) resolveContained(key string) (string, error) {
	if strings.ContainsRune(key, 0) {
		return "", fmt.Errorf("invalid storage key: contains NUL")
	}
	joined := filepath.Join(l.basePath, key)
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("invalid storage key: %w", err)
	}
	base := l.basePath
	if !strings.HasSuffix(base, string(os.PathSeparator)) {
		base += string(os.PathSeparator)
	}
	if abs != l.basePath && !strings.HasPrefix(abs, base) {
		return "", fmt.Errorf("invalid storage key: %q escapes storage root", key)
	}
	return abs, nil
}

func (l *localStorage) Write(_ context.Context, key string, data []byte) error {
	fullPath, err := l.resolveContained(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write file %s: %w", fullPath, err)
	}
	return nil
}

func (l *localStorage) Read(_ context.Context, key string) ([]byte, error) {
	fullPath, err := l.resolveContained(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file not found: %s", key)
		}
		return nil, fmt.Errorf("failed to read file %s: %w", fullPath, err)
	}
	return data, nil
}
