package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// Workspace is a parsed workspace.pekit.toml. Its presence marks a
// directory as a workspace root: members beneath it inherit the sibling
// package.pekit.toml, and `pekit workspace <verb>` fans out over Include.
type Workspace struct {
	// Include is a glob (relative to the root) selecting member recipes.
	Include string
}

// ParseWorkspace parses workspace.pekit.toml source.
func ParseWorkspace(src string) (*Workspace, error) {
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}
	ws := &Workspace{}
	for _, key := range sortedKeys(raw) {
		switch key {
		case "include":
			s, err := stringValue("workspace", key, raw[key])
			if err != nil {
				return nil, err
			}
			ws.Include = s
		default:
			return nil, fmt.Errorf("workspace.pekit.toml: unknown key %q", key)
		}
	}
	if ws.Include == "" {
		return nil, fmt.Errorf("workspace.pekit.toml: missing required key %q", "include")
	}
	return ws, nil
}

// LoadWorkspace reads and parses a workspace.pekit.toml.
func LoadWorkspace(path string) (*Workspace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ws, err := ParseWorkspace(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ws, nil
}

// findWorkspaceRoot walks up from start looking for a workspace.pekit.toml.
// Returns the directory that holds it and true, or ("", false) if none is
// found before the filesystem root. The marker — not a bare ancestor
// package.pekit.toml — is what gates inheritance, so it is never ambient.
func findWorkspaceRoot(start string) (string, bool, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false, err
	}
	for {
		_, err := os.Stat(filepath.Join(dir, "workspace.pekit.toml"))
		if err == nil {
			return dir, true, nil
		}
		if !os.IsNotExist(err) {
			return "", false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

// workspaceMembers expands the include glob (relative to root) to member
// recipe directories — matches that are directories containing a
// pekit.toml. Loose files and output dirs are skipped. Sorted.
func workspaceMembers(root string, ws *Workspace) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, ws.Include))
	if err != nil {
		return nil, fmt.Errorf("workspace include %q: %w", ws.Include, err)
	}
	var members []string
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(m, "pekit.toml")); err == nil {
			members = append(members, m)
		}
	}
	sort.Strings(members)
	return members, nil
}

// latestVersion resolves the single newest version of the recipe in the
// current directory: enumerate upstream tags, apply the [source].versions
// cap, take the semver maximum. Used by `pekit workspace ... --latest`.
func latestVersion() (*Version, error) {
	src, err := loadRecipeSource()
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("--latest needs a [source] to enumerate upstream tags")
	}
	all, err := enumerateVersions(src)
	if err != nil {
		return nil, err
	}
	set := make(map[string]*Version, len(all))
	for _, v := range all {
		set[v.Full] = v
	}
	if src.Versions != "" {
		if _, err := capVersions(set, src.Versions); err != nil {
			return nil, err
		}
	}
	vers := make([]*Version, 0, len(set))
	for _, v := range set {
		vers = append(vers, v)
	}
	if len(vers) == 0 {
		return nil, fmt.Errorf("no versions to pick a latest from")
	}
	sortVersions(vers)
	return vers[len(vers)-1], nil
}
