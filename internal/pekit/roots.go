package pekit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type RecipeLocation struct {
	Root       string
	Path       string
	Workspace  *WorkspaceConfig
	Remote     bool
	RemoteSpec string
}

func locateRecipe(inv Invocation) (RecipeLocation, Invocation, error) {
	localInv := inv
	locator := inv.RecipeFlag
	if locator == "" && len(localInv.Positionals) > 0 && looksRemoteLocator(localInv.Positionals[0]) {
		locator = localInv.Positionals[0]
		localInv.RemoteRecipe = locator
		localInv.Positionals = append([]string(nil), localInv.Positionals[1:]...)
	}
	var root, path string
	remote := false
	if locator != "" {
		if looksRemoteLocator(locator) {
			materialized, err := materializeRemoteLocator(locator, "recipe")
			if err != nil {
				return RecipeLocation{}, inv, err
			}
			root = materialized
			path = filepath.Join(root, "pekit.toml")
			remote = true
		} else {
			resolved, err := absPath(inv.Cwd, locator)
			if err != nil {
				return RecipeLocation{}, inv, wrapDiag("invalid_path", "resolve --recipe", err)
			}
			st, err := os.Stat(resolved)
			if err != nil {
				return RecipeLocation{}, inv, wrapDiag("missing_recipe", resolved, err)
			}
			if st.IsDir() {
				root = resolved
				path = filepath.Join(root, "pekit.toml")
			} else {
				path = resolved
				root = filepath.Dir(path)
			}
		}
	} else {
		found, err := findUp(inv.Cwd, "pekit.toml")
		if err != nil {
			return RecipeLocation{}, inv, err
		}
		path = found
		root = filepath.Dir(path)
	}
	if !fileExists(path) {
		return RecipeLocation{}, inv, diagAt("missing_recipe", path, "recipe file not found")
	}
	var ws *WorkspaceConfig
	if inv.WorkspaceFlag == "" {
		if wsPath, err := findUp(root, "workspace.pekit.toml"); err == nil {
			cfg, err := LoadWorkspace(wsPath)
			if err != nil {
				return RecipeLocation{}, inv, err
			}
			ws = &cfg
		}
	}
	return RecipeLocation{Root: root, Path: path, Workspace: ws, Remote: remote, RemoteSpec: locator}, localInv, nil
}

func locateWorkspace(inv Invocation) (WorkspaceConfig, error) {
	locator := inv.WorkspaceFlag
	if locator != "" {
		if looksRemoteLocator(locator) {
			root, err := materializeRemoteLocator(locator, "workspace")
			if err != nil {
				return WorkspaceConfig{}, err
			}
			return LoadWorkspace(filepath.Join(root, "workspace.pekit.toml"))
		}
		resolved, err := absPath(inv.Cwd, locator)
		if err != nil {
			return WorkspaceConfig{}, wrapDiag("invalid_path", "resolve --workspace", err)
		}
		st, err := os.Stat(resolved)
		if err != nil {
			return WorkspaceConfig{}, wrapDiag("missing_workspace", resolved, err)
		}
		if st.IsDir() {
			return LoadWorkspace(filepath.Join(resolved, "workspace.pekit.toml"))
		}
		return LoadWorkspace(resolved)
	}
	path, err := findUp(inv.Cwd, "workspace.pekit.toml")
	if err != nil {
		return WorkspaceConfig{}, err
	}
	return LoadWorkspace(path)
}

func findUp(start, name string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, name)
		if fileExists(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", diag("not_found", "could not find %s from %s upward", name, start)
		}
		dir = parent
	}
}

func discoverWorkspaceMembers(ws WorkspaceConfig) ([]WorkspaceMember, error) {
	type seenMember struct {
		root string
	}
	seen := map[string]seenMember{}
	excluded := map[string]bool{}
	fsys := os.DirFS(ws.Root)
	for _, pattern := range ws.Exclude {
		matches, err := doublestar.Glob(fsys, filepath.ToSlash(pattern))
		if err != nil {
			return nil, wrapDiag("workspace_glob", pattern, err)
		}
		for _, match := range matches {
			excluded[filepath.Clean(match)] = true
		}
	}
	for _, pattern := range ws.Include {
		matches, err := doublestar.Glob(fsys, filepath.ToSlash(pattern))
		if err != nil {
			return nil, wrapDiag("workspace_glob", pattern, err)
		}
		for _, match := range matches {
			cleaned := filepath.Clean(match)
			if excluded[cleaned] {
				continue
			}
			root := filepath.Join(ws.Root, cleaned)
			if fileExists(filepath.Join(root, "pekit.toml")) {
				seen[filepath.ToSlash(cleaned)] = seenMember{root: root}
			}
		}
	}
	ids := sortedKeys(seen)
	members := make([]WorkspaceMember, 0, len(ids))
	for _, id := range ids {
		members = append(members, WorkspaceMember{ID: id, Root: seen[id].root, RecipePath: filepath.Join(seen[id].root, "pekit.toml")})
	}
	return members, nil
}

func looksRemoteLocator(value string) bool {
	if value == "" || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") {
		return false
	}
	return strings.HasPrefix(value, "github.com/") || strings.Contains(value, "://") || strings.HasSuffix(value, ".git") || strings.Contains(value, "//")
}

type remoteSpec struct {
	URL    string
	Subdir string
	Ref    string
}

func materializeRemoteLocator(locator, kind string) (string, error) {
	spec, err := parseRemote(locator)
	if err != nil {
		return "", err
	}
	cacheRoot := filepath.Join(os.TempDir(), "pekit-remote")
	rawRepo := filepath.Join(cacheRoot, "git", shortHash(spec.URL), "repo.git")
	if !dirExists(rawRepo) {
		if err := os.MkdirAll(filepath.Dir(rawRepo), 0o755); err != nil {
			return "", wrapDiag("mkdir", filepath.Dir(rawRepo), err)
		}
		if err := runSimple("", "git", "clone", "--mirror", spec.URL, rawRepo); err != nil {
			return "", wrapDiag("git_clone", "clone remote "+kind+" "+locator, err)
		}
	} else {
		if err := runSimple(rawRepo, "git", "fetch", "--prune", "--tags"); err != nil {
			return "", wrapDiag("git_fetch", "fetch remote "+kind+" "+locator, err)
		}
	}
	commit, err := resolveRemoteCommit(rawRepo, spec.Ref)
	if err != nil {
		return "", err
	}
	base := filepath.Join(cacheRoot, "checkouts", shortHash(spec.URL, spec.Subdir, commit))
	if !dirExists(base) {
		if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
			return "", wrapDiag("mkdir", filepath.Dir(base), err)
		}
		if err := runSimple("", "git", "clone", rawRepo, base); err != nil {
			return "", wrapDiag("git_clone", "checkout remote "+kind+" "+locator, err)
		}
	}
	if err := runSimple(base, "git", "reset", "--hard", commit); err != nil {
		return "", wrapDiag("git_reset", "reset remote "+kind+" "+locator, err)
	}
	if err := runSimple(base, "git", "clean", "-fdx"); err != nil {
		return "", wrapDiag("git_clean", "clean remote "+kind+" "+locator, err)
	}
	root := base
	if spec.Subdir != "" {
		root = filepath.Join(base, filepath.FromSlash(spec.Subdir))
	}
	return root, nil
}

func parseRemote(locator string) (remoteSpec, error) {
	spec := remoteSpec{}
	beforeRef, ref, hasRef := splitRemoteRef(locator)
	if hasRef {
		spec.Ref = ref
	}
	repoPart := beforeRef
	switch {
	case strings.HasPrefix(repoPart, "github.com/"):
		if idx := strings.Index(repoPart, "//"); idx >= 0 {
			spec.Subdir = strings.TrimPrefix(repoPart[idx+2:], "/")
			repoPart = repoPart[:idx]
		}
		parts := strings.Split(repoPart, "/")
		if len(parts) < 3 {
			return spec, fmt.Errorf("invalid github locator %q", locator)
		}
		spec.URL = "https://" + strings.Join(parts[:3], "/") + ".git"
	case strings.Contains(repoPart, "://"):
		if idx := strings.Index(repoPart, ".git//"); idx >= 0 {
			spec.Subdir = strings.TrimPrefix(repoPart[idx+6:], "/")
			repoPart = repoPart[:idx+4]
		}
		spec.URL = repoPart
	case strings.Contains(repoPart, ".git//"):
		idx := strings.Index(repoPart, ".git//")
		spec.Subdir = strings.TrimPrefix(repoPart[idx+6:], "/")
		spec.URL = repoPart[:idx+4]
	case strings.HasSuffix(repoPart, ".git"):
		spec.URL = repoPart
	case strings.Contains(repoPart, "//"):
		left, right, _ := strings.Cut(repoPart, "//")
		spec.URL = left
		spec.Subdir = strings.TrimPrefix(right, "/")
	default:
		return spec, fmt.Errorf("unsupported remote locator %q", locator)
	}
	return spec, nil
}

func splitRemoteRef(locator string) (string, string, bool) {
	idx := strings.LastIndex(locator, "@")
	if idx <= 0 || idx == len(locator)-1 {
		return locator, "", false
	}
	before := locator[:idx]
	after := locator[idx+1:]
	if strings.HasPrefix(before, "git@") && !strings.Contains(before, ".git") {
		return locator, "", false
	}
	if strings.Contains(before, ".git") || strings.HasPrefix(before, "github.com/") || strings.Contains(before, "//") {
		return before, after, true
	}
	return locator, "", false
}

func resolveRemoteCommit(rawRepo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	candidates := []string{ref}
	if ref != "HEAD" {
		candidates = append(candidates, "refs/heads/"+ref, "refs/tags/"+ref)
	}
	var lastErr error
	for _, candidate := range candidates {
		out, err := commandOutput(rawRepo, "git", "rev-parse", candidate+"^{commit}")
		if err == nil {
			return strings.TrimSpace(out), nil
		}
		lastErr = err
	}
	return "", wrapDiag("git_resolve", "resolve remote recipe ref "+ref, lastErr)
}

func runSimple(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
		}
		return err
	}
	return nil
}
