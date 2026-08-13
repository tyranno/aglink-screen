package main

import (
	"os"
	"path/filepath"
	"strings"
)

// dataDir returns the aglink data directory, creating it if necessary:
// $AGLINK_HOME if set, else ~/.aglink. The host process owns one-time legacy
// ~/.teleclaude migration; helper apps should not split back to the old dir.
func dataDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("AGLINK_HOME")); v != "" {
		if err := os.MkdirAll(v, 0o700); err != nil {
			return "", err
		}
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".aglink")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
