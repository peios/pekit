package pekit

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// patchSet is a recipe's patch series, loaded and validated: the ordered
// entries from the series file and a content hash over them. The hash is
// empty when the series lists nothing, so a recipe with an (temporarily)
// empty series shares its materialisation scope with an unpatched one.
type patchSet struct {
	Dir     string   // absolute patches directory
	Name    string   // configured directory name, for messages
	Entries []string // series order, slash-separated relative paths
	Hash    string   // content hash of the ordered series; "" when empty
}

// loadPatchSet reads and validates [source].patches for a recipe. Returns
// nil when the recipe declares no patches. Every series entry must exist;
// a *.patch file in the directory that the series does not list is an
// error unless allowUnused, so the shipped series is always the applied
// series.
func loadPatchSet(recipe RecipeConfig, allowUnused bool) (*patchSet, error) {
	name := recipe.Source.Patches
	if name == "" {
		return nil, nil
	}
	dir := filepath.Join(recipe.Root, name)
	seriesPath := filepath.Join(dir, "series")
	data, err := os.ReadFile(seriesPath)
	if err != nil {
		return nil, wrapDiag("patch_series", seriesPath, err)
	}
	var entries []string
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		entry := line
		if idx := strings.Index(entry, "#"); idx >= 0 {
			entry = entry[:idx]
		}
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.ContainsAny(entry, " \t") {
			return nil, diag("patch_series", "%s line %d: entry %q contains whitespace", seriesPath, i+1, entry)
		}
		clean, err := cleanRelPath(entry)
		if err != nil {
			return nil, diag("patch_series", "%s line %d: %v", seriesPath, i+1, err)
		}
		if seen[clean] {
			return nil, diag("patch_series", "%s lists %s twice", seriesPath, clean)
		}
		seen[clean] = true
		if !fileExists(filepath.Join(dir, filepath.FromSlash(clean))) {
			return nil, diag("patch_missing", "series entry %s not found under %s", clean, name)
		}
		entries = append(entries, clean)
	}
	var unused []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".patch") {
			return walkErr
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if !seen[filepath.ToSlash(rel)] {
			unused = append(unused, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, wrapDiag("patch_series", dir, err)
	}
	if len(unused) > 0 && !allowUnused {
		sort.Strings(unused)
		return nil, diag("unused_patch", "not listed in %s/series: %s (--allow-unused to permit)", name, strings.Join(unused, ", "))
	}
	ps := &patchSet{Dir: dir, Name: name, Entries: entries}
	if len(entries) > 0 {
		parts := make([]string, 0, 2*len(entries))
		for _, entry := range entries {
			body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(entry)))
			if err != nil {
				return nil, wrapDiag("patch_series", entry, err)
			}
			parts = append(parts, entry, string(body))
		}
		ps.Hash = shortHash(parts...)
	}
	return ps, nil
}

// applyPatchSet applies the series to a materialised source tree with
// `git apply`, in series order, strictly: no fuzz, and a patch that
// matches nothing is a hard failure.
func applyPatchSet(ctx *Context, ps *patchSet, tree string, version Version) error {
	if ps == nil || len(ps.Entries) == 0 {
		return nil
	}
	for _, entry := range ps.Entries {
		cmd := exec.Command("git", "apply", "--whitespace=nowarn", filepath.Join(ps.Dir, filepath.FromSlash(entry)))
		cmd.Dir = tree
		// Without a ceiling, `git apply` in a tree that is not its own git
		// root resolves an ancestor repo (the pkgs checkout, for out/ trees)
		// and treats patches whose target paths are absent there as
		// "Skipped" — while still exiting 0. Stop the upward search at the
		// tree's parent.
		cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(tree))
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return diag("patch_apply", "apply %s: %s", entry, msg)
		}
		if strings.Contains(string(out), "Skipped patch") {
			return diag("patch_skipped", "%s matched no files in the source tree", entry)
		}
	}
	ctx.Renderer.Event(Event{Type: "patch", Version: version.Raw, Path: ps.Dir, Message: fmt.Sprintf("applied %d patches from %s/series", len(ps.Entries), ps.Name)})
	return nil
}
