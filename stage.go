package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// prepareOutDir readies the staging directory for one target of one step
// (outDir/<step>/<name>), optionally wiping it first, and returns its
// absolute path for $PEKIT_OUT. Scoping the stage by step keeps each
// step's artifacts (and wipes) isolated from the others'.
func prepareOutDir(outDir, step, name string, clear bool) (string, error) {
	dir := filepath.Join(outDir, step, name)
	if clear {
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("clearing out dir %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating out dir %s: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving out dir %s: %w", dir, err)
	}
	return abs, nil
}

// isEmptyDir reports whether dir exists and contains nothing.
func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// dirHasEntries reports whether dir exists and contains at least one entry.
func dirHasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
