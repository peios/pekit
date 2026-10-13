package pekit

import (
	"fmt"
	"path"
	"strings"
)

// Exclusions are literal, canonical paths below the selected source root.
// Requiring canonical spelling avoids traversal, glob and template ambiguity.
func validateArchiveExclusions(excludes []string) error {
	for _, name := range excludes {
		if name == "" || name == "." || name == ".." || path.IsAbs(name) ||
			path.Clean(name) != name || strings.HasPrefix(name, "../") ||
			strings.ContainsAny(name, "\\\x00*?[]{}") {
			return fmt.Errorf("exclude %q must be a canonical relative file or directory path without glob or template syntax", name)
		}
	}
	return nil
}

// Call only after validating the member name. An excluded entry is never
// created, so its type, contents or link target need not be materialised.
func archiveEntryExcluded(rel string, excludes []string) bool {
	for _, exclude := range excludes {
		if rel == exclude || strings.HasPrefix(rel, exclude+"/") {
			return true
		}
	}
	return false
}
