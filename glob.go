package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// hasGlobMeta reports whether p contains a glob metacharacter doublestar
// recognises (*, ?, [, {). A [files] source with one is expanded against the
// stage; without one it is the single-file case.
func hasGlobMeta(p string) bool {
	return strings.ContainsAny(p, "*?[{")
}

// excludedBy returns the index of the first exclude pattern that matches a
// staged source, or -1 if none. target is the staged file's build target ("" for
// a literal source) and relPath is its source path relative to that stage,
// slash-separated. An exclude matches when its target is the same and its path
// pattern (doublestar, so plain paths match exactly and "*"/"**" expand)
// matches relPath — the source side, mirroring the ":usr/bin/..." syntax.
func excludedBy(excludes []SourceRef, target, relPath string) int {
	for i, ex := range excludes {
		if ex.Target != target {
			continue
		}
		if ok, err := doublestar.Match(ex.Path, relPath); err == nil && ok {
			return i
		}
	}
	return -1
}

// expandSource resolves one [files] mapping (src -> dest) against root — the
// on-disk directory the source is relative to: a build target's stage
// (outDir/build/<target>), or the project/source root for a literal path.
//
// A wildcard-free source is the single-file case: src names one file and dest
// is its archive path, unchanged from pekit's original behaviour. A source
// with a glob metacharacter is expanded — every matched regular file or
// symlink is included, and its archive path is dest joined with the match's
// path relative to the glob's wildcard-free base. So dest is a directory
// prefix, and base == dest means "keep the staged path". Directories are not
// emitted (pack creates the ancestors of files automatically); an empty match
// is an error, exactly like a missing literal file.
func expandSource(root string, src SourceRef, dest, pkgName string) ([]StagedFile, error) {
	if !hasGlobMeta(src.Path) {
		abs, err := filepath.Abs(filepath.Join(root, src.Path))
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			hint := ""
			if src.Target != "" {
				hint = fmt.Sprintf(" (run %q first?)", "pekit build "+src.Target)
			}
			return nil, fmt.Errorf("package %s: source %q not found at %s%s", pkgName, src, abs, hint)
		}
		return []StagedFile{{Source: abs, Dest: dest}}, nil
	}

	// Split off the wildcard-free base so os.DirFS has a concrete root, then
	// match the remaining pattern beneath it. Matches come back relative to
	// the base, which is exactly the rebase basis: each is laid under dest.
	base, pattern := doublestar.SplitPattern(src.Path)
	baseAbs, err := filepath.Abs(filepath.Join(root, base))
	if err != nil {
		return nil, err
	}
	// WithNoFollow: package a symlink as itself rather than traversing into
	// the tree it points at (and avoid symlink-cycle surprises under **).
	matches, err := doublestar.Glob(os.DirFS(baseAbs), pattern, doublestar.WithNoFollow())
	if err != nil {
		return nil, fmt.Errorf("package %s: source %q: %w", pkgName, src, err)
	}

	var staged []StagedFile
	for _, rel := range matches {
		real := filepath.Join(baseAbs, filepath.FromSlash(rel))
		info, err := os.Lstat(real)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue // ancestors are emitted by pack from the files within
		}
		staged = append(staged, StagedFile{Source: real, Dest: path.Join(dest, rel)})
	}
	if len(staged) == 0 {
		return nil, fmt.Errorf("package %s: source %q matched no files under %s", pkgName, src, baseAbs)
	}
	sort.Slice(staged, func(i, j int) bool { return staged[i].Dest < staged[j].Dest })
	return staged, nil
}
