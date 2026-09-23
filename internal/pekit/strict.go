package pekit

import (
	"os/exec"
	"strings"
)

// preflightStrict holds a --strict run to a reviewed catalogue: the workspace
// (or, outside one, the recipe) must be a git checkout with nothing
// uncommitted. Lock files are the exception. `pekit lock --latest` rewrites
// them unattended, and every entry is itself verified against upstream, so a
// newly discovered release does not wait on a human commit. The bypass flags
// are refused at parse time and the source's anchoring once it is resolved.
func preflightStrict(recipe RecipeConfig, workspace *WorkspaceConfig) error {
	root := recipe.Root
	if workspace != nil {
		root = workspace.Root
	}
	status, err := catalogueStatus(root)
	if err != nil {
		return diag("strict_catalogue", "--strict needs the catalogue at %s to be a git checkout: %v", root, err)
	}
	if status != "" {
		lines := strings.Split(status, "\n")
		if len(lines) > 5 {
			lines = append(lines[:5], "...")
		}
		return diag("strict_dirty", "--strict needs a committed catalogue; uncommitted changes in %s:\n%s", root, strings.Join(lines, "\n"))
	}
	return nil
}

// catalogueStatus lists uncommitted changes under root, lock files excepted.
func catalogueStatus(root string) (string, error) {
	cmd := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=normal", "--", ".", ":(exclude)**/pekit.lock", ":(exclude)pekit.lock")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
