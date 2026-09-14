package pekit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// sourceInputs is coordinator-owned. Workers get separate mutable recipe
// copies and read-only helper mounts, never this publication snapshot.
type sourceInputs struct {
	Missing     []string            `json:"missing"`
	Directory   string              `json:"directory"`
	Recipe      string              `json:"recipe"`
	Files       map[string]string   `json:"files"`
	Directories map[string][]string `json:"directories"`
	Excluded    map[string]bool     `json:"excluded"`
	OutBase     string              `json:"out_base"`
}

func sourceInputExcluded(path, outBase string, secrets map[string]bool) bool {
	name := filepath.Base(path)
	return path == outBase || secrets[path] || name == ".git" || name == ".pekit" || name == ".pekit-job.lock" || name == ".env" || strings.HasPrefix(name, ".env.") || name == "credentials" || name == "credentials.toml" || name == "__pycache__" || strings.HasSuffix(name, ".keyring.pekit.toml")
}

func inputIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		return "link:" + link, err
	}
	if info.IsDir() {
		return fmt.Sprintf("dir:%04o", info.Mode().Perm()), nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("unsupported source input: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%04o:%s", info.Mode().Perm(), hex.EncodeToString(h.Sum(nil))), nil
}

func (s *sourceInputs) unchanged() error {
	for _, path := range s.Missing {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return diag("source_input_changed", "previously absent build policy file appeared: %s; rebuild before packaging", path)
		}
	}
	for path, expected := range s.Directories {
		got, err := sourceInputChildren(path, s.OutBase, s.Excluded)
		if err != nil || strings.Join(got, "\x00") != strings.Join(expected, "\x00") {
			return diag("source_input_changed", "build input directory changed during or since this job: %s; rebuild before packaging", path)
		}
	}

	for _, path := range sortedKeys(s.Files) {
		got, err := inputIdentity(path)
		if err != nil || got != s.Files[path] {
			return diag("source_input_changed", "build-controlling input changed during or since this job: %s; rebuild before packaging", path)
		}
	}
	return nil
}

// captureSourceInputs keeps the original relative workspace topology. It copies
// entire recipe support trees, excludes local state/secrets, and follows only
// explicitly selected workspace inputs and safe links to selected policy files.
func captureSourceInputs(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, directory string, secrets map[string]bool, helpers map[string]string) (*sourceInputs, error) {
	ignoreRoot := recipe.Root
	if workspace != nil {
		ignoreRoot = workspace.Root
	}
	secrets = sourceSnapshotExclusions(ignoreRoot, secrets)
	s := &sourceInputs{Directory: directory, Files: map[string]string{}, Directories: map[string][]string{}, Excluded: secrets, OutBase: source.OutBase}
	base := recipe.Root
	member := filepath.Base(recipe.Root)
	if workspace == nil && len(recipe.SourcePackage.WorkspaceInputs) > 0 {
		return nil, diag("missing_workspace", "source_package.workspace_inputs requires a workspace")
	}
	if workspace != nil {
		base = workspace.Root
		rel, err := filepath.Rel(base, recipe.Root)
		if err != nil || !withinDirectory(base, recipe.Root) {
			return nil, diag("source_bundle_layout", "source bundles require a recipe within its workspace root")
		}
		member = rel
	}
	s.Recipe = filepath.ToSlash(filepath.Join("workspace", member))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	copied := map[string]bool{}
	var copyPath func(string, string, string) error
	copyPath = func(original, physical, dest string) error {
		if sourceInputExcluded(original, source.OutBase, secrets) {
			return nil
		}
		if copied[dest] {
			return nil
		}
		copied[dest] = true
		info, err := os.Lstat(physical)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if physical == original {
				children, err := sourceInputChildren(original, source.OutBase, secrets)
				if err != nil {
					return err
				}
				s.Directories[original] = children
			}
			if err := os.MkdirAll(dest, info.Mode().Perm()|0700); err != nil {
				return err
			}
			children, err := os.ReadDir(physical)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := copyPath(filepath.Join(original, child.Name()), filepath.Join(physical, child.Name()), filepath.Join(dest, child.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		identity, err := inputIdentity(physical)
		if err != nil {
			return err
		}
		if physical == original {
			s.Files[original] = identity
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(physical)
			if err != nil {
				return err
			}
			target := filepath.Clean(filepath.Join(filepath.Dir(original), link))
			if filepath.IsAbs(link) || !withinDirectory(base, target) || sourceInputExcluded(target, source.OutBase, secrets) {
				return diag("source_input_escape", "source input link %s points outside exported inputs", original)
			}
			// Links may reach shared profile files, but never silently import another
			// recipe or an arbitrary workspace subtree.
			sharedTarget := false
			if workspace != nil {
				for _, rel := range append(append(append([]string{}, workspace.SourceInputs...), workspace.Isolation.Inputs...), recipe.SourcePackage.WorkspaceInputs...) {
					if withinDirectory(filepath.Join(base, rel), target) {
						sharedTarget = true
					}
				}
			}
			if !withinDirectory(recipe.Root, target) && !sharedTarget {
				name := filepath.Base(target)
				if !strings.HasSuffix(name, ".env.pekit.toml") && name != "lint.pekit.toml" && name != "package.pekit.toml" {
					return diag("source_input_escape", "declare the shared input containing symlink target %s", target)
				}
				rel, _ := filepath.Rel(base, target)
				if err := copyPath(target, target, filepath.Join(directory, "workspace", rel)); err != nil {
					return err
				}
			}
			return os.Symlink(link, dest)
		}
		in, err := os.Open(physical)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}
	if err := copyPath(recipe.Root, recipe.Root, filepath.Join(directory, filepath.FromSlash(s.Recipe))); err != nil {
		return nil, err
	}
	if workspace != nil {
		names := []string{"package.pekit.toml", "lint.pekit.toml", "env.pekit.toml"}
		if ctx.Inv.EnvName != "" && ctx.Inv.EnvName != "none" && ctx.Inv.EnvName != "main" {
			names = append(names, ctx.Inv.EnvName+".env.pekit.toml")
		}
		for _, name := range names {
			path := filepath.Join(workspace.Root, name)
			if _, err := os.Lstat(path); os.IsNotExist(err) {
				s.Missing = append(s.Missing, path)
			}
		}
		children, err := os.ReadDir(workspace.Root)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			name := child.Name()
			if name == "workspace.pekit.toml" || name == "package.pekit.toml" || name == "lint.pekit.toml" || name == "env.pekit.toml" || strings.HasSuffix(name, ".env.pekit.toml") || strings.HasSuffix(name, ".lint.pekit.toml") {
				path := filepath.Join(base, name)
				if err := copyPath(path, path, filepath.Join(directory, "workspace", name)); err != nil {
					return nil, err
				}
			}
		}
		shared := append(append([]string{}, workspace.SourceInputs...), workspace.Isolation.Inputs...)
		shared = append(shared, recipe.SourcePackage.WorkspaceInputs...)
		for _, rel := range shared {
			path := filepath.Join(base, rel)
			if err := validateSourceInputPath(base, rel); err != nil {
				return nil, err
			}
			if sourceInputExcluded(path, source.OutBase, secrets) {
				return nil, diag("invalid_source_input", "declared source input is ignored or excluded: %s", rel)
			}
			physical := path
			if snapshot, ok := helpers[path]; ok {
				physical = snapshot
			}
			if err := copyPath(path, physical, filepath.Join(directory, "workspace", rel)); err != nil {
				return nil, err
			}
		}
	}
	// Resolve links only inside the completed export; missing and excluded
	// targets fail instead of producing a source package that cannot rebuild.
	err := filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !withinDirectory(directory, real) {
			return diag("source_input_escape", "missing or escaping exported symlink: %s", path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.unchanged(); err != nil {
		return nil, err
	}
	return s, nil
}

func validateSourceInputPath(root, rel string) error {
	clean, err := cleanRelPath(rel)
	if err != nil || clean != rel || rel == "." {
		return diag("invalid_source_input", "source input must be a non-empty canonical relative path: %q", rel)
	}
	path := filepath.Join(root, rel)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return wrapDiag("missing_source_input", path, err)
	}
	if real == root || !withinDirectory(root, real) {
		return diag("source_input_escape", "source input escapes workspace: %s", rel)
	}
	return nil
}

func saveSourceInputs(path string, s *sourceInputs) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func sourceInputChildren(path, outBase string, secrets map[string]bool) ([]string, error) {
	items, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, item := range items {
		if !sourceInputExcluded(filepath.Join(path, item.Name()), outBase, secrets) {
			names = append(names, item.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func sourceSnapshotExclusions(root string, secrets map[string]bool) map[string]bool {
	excluded := map[string]bool{}
	for path, value := range secrets {
		excluded[path] = value
	}
	// Gitignored developer files are neither worker inputs nor published source.
	ignored, err := exec.Command("git", "-C", root, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z", "--", ".").Output()
	if err == nil {
		for _, rel := range strings.Split(string(ignored), "\x00") {
			if rel != "" {
				excluded[filepath.Join(root, strings.TrimSuffix(rel, "/"))] = true
			}
		}
	}
	return excluded
}
