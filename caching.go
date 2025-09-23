package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// GetCacheFilePath resolves ~/.cache/.pypi-tracer.cache
func GetCacheFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home dir: %w", err)
	}
	cacheDir := filepath.Join(home, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create cache dir: %w", err)
	}
	return filepath.Join(cacheDir, ".pypi-tracer.cache"), nil
}
