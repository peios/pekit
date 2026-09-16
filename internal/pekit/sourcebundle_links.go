package pekit

import (
	"os"
	"path/filepath"
	"strings"
)

// sourceLinkContained resolves each component before applying subsequent ..
// components. Missing internal targets are valid upstream fixtures. Absolute
// links, cycles, non-directory parents and even temporary escapes are not.
func sourceLinkContained(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	pending := strings.Split(rel, string(filepath.Separator))
	parts := []string{}
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				return false
			}
			parts = parts[:len(parts)-1]
			continue
		}
		candidate := filepath.Join(append([]string{root}, append(parts, part)...)...)
		info, err := os.Lstat(candidate)
		if err != nil && !os.IsNotExist(err) {
			return false
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			hops++
			target, err := os.Readlink(candidate)
			if err != nil || hops > 40 || filepath.IsAbs(target) {
				return false
			}
			pending = append(strings.Split(target, string(filepath.Separator)), pending...)
			continue
		}
		if err == nil && len(pending) > 0 && !info.IsDir() {
			return false
		}
		parts = append(parts, part)
	}
	return true
}
