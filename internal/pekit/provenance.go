package pekit

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// packRun carries the per-run packaging context every written artifact
// shares: the signing key and the provenance identity of the producing
// run (manifest §3.3.4 recipe_ref / builder).
type packRun struct {
	SignKey   ed25519.PrivateKey
	RecipeRef string
	Builder   string
}

// recipeScope names the paths whose worktree state a recipe_ref's
// "+dirty" marker describes: the effective inputs of the recipe being
// built, minus the managed output they produce.
//
// The marker used to reflect the whole enclosing work tree, which in a
// shared recipe repository such as pkgs meant that editing any package
// tainted the provenance of every other one, and that an operational
// output or repository link beside a member tainted an otherwise exact
// commit (PEI-743). Scoping it keeps the marker about the build: a
// commit id, plus an answer to "did anything that could change this
// artifact differ from that commit?".
type recipeScope struct {
	// Include holds absolute paths whose changes can change the build:
	// the selected recipe — its recipe and package files, source
	// patches, embedded keys and lock — and, in a workspace, the
	// inherited workspace, environment and keyring inputs beside it.
	Include []string
	// Exclude holds absolute paths inside those that cannot: the other
	// members of the workspace, and the managed output and repository
	// conduits — a member's out_dir, whether it is a real directory or a
	// link into shared storage, and the publish destinations.
	Exclude []string
}

// recipeRefScope builds the scope for one packaging run. Sibling recipes
// are found the way workspace fan-out finds its members, except that the
// workspace's own `exclude` is not applied: a recipe kept out of fan-out
// is still somebody else's recipe, and its edits are still not this
// build's inputs.
func recipeRefScope(recipe RecipeConfig, workspace *WorkspaceConfig, instances []PackageInstance) recipeScope {
	scope := recipeScope{Include: []string{recipe.Root}, Exclude: []string{recipeOutBase(recipe)}}
	if workspace != nil && workspace.Root != recipe.Root {
		scope.Include = append(scope.Include, workspace.Root)
		for _, sibling := range workspaceRecipeRoots(*workspace) {
			if isPathWithin(recipe.Root, sibling) {
				continue
			}
			scope.Exclude = append(scope.Exclude, sibling)
		}
	}
	scope.Exclude = append(scope.Exclude, publishConduits(workspace, recipe, instances)...)
	return scope
}

// workspaceRecipeRoots lists every directory the workspace's include
// globs reach that holds a recipe of its own.
func workspaceRecipeRoots(ws WorkspaceConfig) []string {
	var roots []string
	fsys := os.DirFS(ws.Root)
	for _, pattern := range ws.Include {
		matches, err := doublestar.Glob(fsys, filepath.ToSlash(pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			root := filepath.Join(ws.Root, filepath.Clean(match))
			if fileExists(filepath.Join(root, "pekit.toml")) {
				roots = append(roots, root)
			}
		}
	}
	return roots
}

// publishConduits names the directories a run publishes into. They are
// pekit-managed output, usually shared at the workspace root, so their
// state says nothing about the recipe that produced what lands in them.
func publishConduits(workspace *WorkspaceConfig, recipe RecipeConfig, instances []PackageInstance) []string {
	var dirs []string
	for _, inst := range instances {
		for _, target := range inst.Config.Publish.LocalDir {
			if dst, _, err := renderLocalDirDestination(workspace, recipe, inst, target); err == nil {
				dirs = append(dirs, filepath.Dir(dst))
			}
		}
		if target := inst.Config.Publish.Peipkg; target != nil {
			if dir, _, err := renderPeipkgRepository(workspace, recipe, *target); err == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

func recipeOutBase(recipe RecipeConfig) string {
	out := recipe.OutDir
	if !filepath.IsAbs(out) {
		out = filepath.Join(recipe.Root, out)
	}
	return out
}

// isPathWithin reports whether path is candidate itself or below it.
func isPathWithin(path, candidate string) bool {
	rel, err := filepath.Rel(candidate, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// recipeRef pins the recipe tree's VCS identity for the manifest build
// block: "git:<commit>", with "+dirty" appended when anything in the
// scope differs from that commit — a dirty scope means the commit alone
// does not describe the build's inputs, so over-marking beats lying.
// Empty when the recipe is not inside a git work tree. Unlike source
// resolution, this deliberately resolves the enclosing repository: a
// workspace member's identity is the workspace repo's commit.
func recipeRef(root string, scope recipeScope) string {
	commit, err := commandOutput(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	ref := "git:" + strings.TrimSpace(commit)
	if scopeDirty(root, scope) {
		ref += "+dirty"
	}
	return ref
}

// scopeDirty asks git about the scope, narrowing to the recipe alone and
// then to the whole work tree if git rejects the pathspecs: a scope that
// reaches outside the repository is one git cannot answer, and the safe
// answer is the wider one.
func scopeDirty(root string, scope recipeScope) bool {
	for _, pathspecs := range [][]string{scopePathspecs(root, scope), {"."}, nil} {
		args := append([]string{"status", "--porcelain", "--"}, pathspecs...)
		status, err := commandOutput(root, "git", args...)
		if err == nil {
			return strings.TrimSpace(status) != ""
		}
	}
	return true
}

// scopePathspecs renders the scope as git pathspecs relative to root,
// the directory the status command runs in. An exclusion only means
// anything inside an inclusion, so paths outside every included path are
// dropped rather than passed to git.
func scopePathspecs(root string, scope recipeScope) []string {
	var specs []string
	for _, path := range scope.Include {
		if rel, ok := relPathspec(root, path); ok {
			specs = append(specs, rel)
		}
	}
	if len(specs) == 0 {
		return []string{"."}
	}
	for _, path := range scope.Exclude {
		if !withinAny(path, scope.Include) {
			continue
		}
		if rel, ok := relPathspec(root, path); ok {
			specs = append(specs, ":(exclude)"+rel)
		}
	}
	return specs
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if isPathWithin(path, root) {
			return true
		}
	}
	return false
}

func relPathspec(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// pekitBuilder identifies this pekit binary for the manifest build
// block: "pekit/<vcs-revision>" (12 hex chars, "+dirty" when built from
// a modified tree), falling back to the module version when the binary
// carries no VCS stamp.
func pekitBuilder() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "pekit/unknown"
	}
	var revision string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += "+dirty"
		}
		return "pekit/" + revision
	}
	version := strings.Trim(info.Main.Version, "()")
	if version == "" {
		version = "unknown"
	}
	return "pekit/" + version
}
