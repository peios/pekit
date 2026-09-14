package pekit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type sourceBundleFile struct {
	Path     string `json:"path"`
	Identity string `json:"identity"`
}
type sourceBundleManifest struct {
	Schema          int                `json:"schema"`
	Recipe          string             `json:"recipe"`
	SourceVersion   string             `json:"source_version"`
	SourceRef       string             `json:"source_ref"`
	SourceTimestamp int64              `json:"source_timestamp"`
	Environment     string             `json:"environment"`
	Builder         string             `json:"builder"`
	Files           []sourceBundleFile `json:"files"`
}

func prepareSourceBundle(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState) (*sourceInputs, error) {
	var inputs *sourceInputs
	if workspace != nil && workspace.Isolation.Enabled {
		job, err := (&sandboxCommand{Recipe: recipe, Workspace: *workspace, Source: source}).job(ctx)
		if err != nil {
			return nil, err
		}
		inputs = job.SourceInputs
	} else {
		directory := filepath.Join(source.WorkBase, ".source-inputs")
		if err := os.RemoveAll(directory); err != nil {
			return nil, err
		}
		// Keyring files never ship. Nonstandard key filenames are also excluded
		// whenever the coordinator was given their paths.
		secrets := map[string]bool{}
		rings, err := resolveKeyrings(ctx.Inv, recipe.Root, workspace)
		if err != nil {
			return nil, err
		}
		for _, value := range rings {
			if path, err := absPath(ctx.Inv.Cwd, value); err == nil {
				secrets[path] = true
			}
		}
		inputs, err = captureSourceInputs(ctx, recipe, workspace, source, directory, secrets, nil)
		if err != nil {
			return nil, err
		}
	}
	// Capture the prepared tree before any package target. The pristine archive
	// remains separately shipped; local reconstruction never reapplies patches.
	prepared := filepath.Join(inputs.Directory, "source")
	if err := os.RemoveAll(prepared); err != nil {
		return nil, err
	}
	secrets := map[string]bool{}
	if job := ctx.Jobs[source.WorkBase]; job != nil {
		secrets = job.Secrets
	}
	if err := snapshotTree(source.SourceRoot, prepared, source.OutBase, secrets); err != nil {
		return nil, err
	}
	return inputs, nil
}

func bundleEntries(ctx *Context, inputs *sourceInputs, source SourceState, version Version, root, stage string, upstream []payloadEntry) ([]payloadEntry, error) {
	if inputs == nil {
		return nil, diag("source_inputs_missing", "source package has no captured build inputs")
	}
	if err := inputs.unchanged(); err != nil {
		return nil, err
	}
	entries, err := sourceTreeEntries(inputs.Directory, root)
	if err != nil {
		return nil, err
	}
	// Snapshot storage contains only the relocatable exported tree. Validate
	// every link again, including upstream source links, before publication.
	for _, entry := range entries {
		info, err := os.Lstat(entry.Source)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(entry.Source)
			link, _ := os.Readlink(entry.Source)
			if err != nil || filepath.IsAbs(link) || !withinDirectory(inputs.Directory, real) {
				return nil, diag("source_input_escape", "source bundle contains a missing or escaping link: %s", entry.Dest)
			}
		}
	}
	// Keep the historical recipe/ and patches/ views for consumers of schema 1.
	recipeRoot := filepath.Join(inputs.Directory, filepath.FromSlash(inputs.Recipe))
	recipeFiles, err := sourceTreeEntries(recipeRoot, root+"/recipe")
	if err != nil {
		return nil, err
	}
	// Materialise profile links in the compatibility view so they cannot become
	// dangling after the workspace topology is removed from their prefix.
	for i := range recipeFiles {
		if st, err := os.Lstat(recipeFiles[i].Source); err == nil && st.Mode()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(recipeFiles[i].Source)
			if err != nil {
				return nil, err
			}
			recipeFiles[i].Source = real
		}
	}
	entries = append(entries, recipeFiles...)
	capturedRecipe, err := LoadRecipe(filepath.Join(recipeRoot, "pekit.toml"))
	if err != nil {
		return nil, err
	}
	if capturedRecipe.Source.Patches != "" {
		patches, err := sourceTreeEntries(filepath.Join(recipeRoot, capturedRecipe.Source.Patches), root+"/patches")
		if err != nil {
			return nil, err
		}
		entries = append(entries, patches...)
	}
	// Include dependency versions and artifact hashes recorded by root preparers.
	if job := ctx.Jobs[source.WorkBase]; job != nil && dirExists(filepath.Join(job.Directory, "dependencies")) {
		deps, err := sourceTreeEntries(filepath.Join(job.Directory, "dependencies"), root+"/build-environment")
		if err != nil {
			return nil, err
		}
		entries = append(entries, deps...)
	}
	if job := ctx.Jobs[source.WorkBase]; job != nil && dirExists(filepath.Join(job.Directory, "acquisition")) {
		acquired, err := sourceTreeEntries(filepath.Join(job.Directory, "acquisition"), root+"/acquisition")
		if err != nil {
			return nil, err
		}
		for _, entry := range acquired {
			if st, err := os.Lstat(entry.Source); err != nil {
				return nil, err
			} else if st.Mode()&os.ModeSymlink != 0 {
				real, err := filepath.EvalSymlinks(entry.Source)
				if err != nil || !withinDirectory(filepath.Join(job.Directory, "acquisition"), real) {
					return nil, diag("source_input_escape", "escaping acquisition input: %s", entry.Dest)
				}
			}
		}
		entries = append(entries, acquired...)
	}
	manifest := sourceBundleManifest{Schema: 2, Recipe: inputs.Recipe, SourceVersion: version.Raw, SourceRef: source.ProvenanceRef, SourceTimestamp: source.Timestamp, Environment: ctx.Inv.EnvName, Builder: pekitBuilder()}
	entries = append(entries, upstream...)
	for _, entry := range entries {
		identity, err := inputIdentity(entry.Source)
		if err != nil {
			return nil, err
		}
		manifest.Files = append(manifest.Files, sourceBundleFile{Path: strings.TrimPrefix(entry.Dest, root+"/"), Identity: identity})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(stage, "build-inputs.json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0644); err != nil {
		return nil, err
	}
	scriptPath := filepath.Join(stage, "rebuild.py")
	if err := os.WriteFile(scriptPath, []byte(sourceRebuildScript), 0755); err != nil {
		return nil, err
	}
	readmePath := filepath.Join(stage, "REBUILD.md")
	readme := fmt.Sprintf("# Rebuilding %s\n\nThis schema-2 bundle contains pristine upstream archives, the prepared source tree (with patches already applied), the complete recipe tree, inherited workspace policy, declared shared inputs and build-environment records. File modes, link targets and hashes are listed in build-inputs.json. Verify the outer .peipkg signature using your distribution trust configuration before trusting this bundle.\n\nRun `python3 rebuild.py test` or `python3 rebuild.py package`. Additional Pekit flags may follow; PEKIT_REBUILD_ENV selects a captured environment (default: the original profile). A compatible Pekit, Python 3, Bubblewrap and the selected profile's root-preparation tools must be installed. The native profile needs a trusted repository at workspace/_peipkgRepo_; the Debian profile needs Docker and archive access. These are declared dependency services, not missing recipe sources. Signing, when required, uses your own operator-provided keys. Production private keys are never distributed.\n\nReconstruction uses the included prepared source with --local and the recorded upstream version. It never fetches upstream or reapplies patches. Local reconstruction artifacts carry local provenance; republishing them as original signed releases is not implied. When this job ran build.vendor, acquisition/vendor contains the captured vendored outputs as well. The generic reconstruction command may still run the declared acquisition stage; these captured sources are available for offline language-specific replay. Exact historic dependency replay and bit-for-bit package reproducibility are separate release checks; the included environment records identify what the original job actually used. New normal jobs continue automatic upstream tracking.\n", version.Raw)
	if err := os.WriteFile(readmePath, []byte(readme), 0644); err != nil {
		return nil, err
	}
	entries = append(entries, payloadEntry{Source: manifestPath, Dest: root + "/build-inputs.json"}, payloadEntry{Source: scriptPath, Dest: root + "/rebuild.py"}, payloadEntry{Source: readmePath, Dest: root + "/REBUILD.md"})
	return entries, nil
}

const sourceRebuildScript = `#!/usr/bin/env python3
"""Verify and rebuild a Pekit corresponding-source bundle without its checkout."""
import hashlib, json, os, pathlib, stat, sys
root = pathlib.Path(__file__).resolve().parent
manifest = json.loads((root / "build-inputs.json").read_text())
if manifest.get("schema") != 2:
    sys.exit("unsupported source bundle schema")
restore_modes = []
for entry in manifest["files"]:
    relative = pathlib.PurePosixPath(entry["path"])
    if relative.is_absolute() or ".." in relative.parts:
        sys.exit("invalid source bundle path")
    path = root / relative
    if not path.resolve().is_relative_to(root):
        sys.exit("source bundle path escapes: " + str(relative))
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode):
        identity = "link:" + os.readlink(path)
    elif stat.S_ISDIR(info.st_mode):
        kind, mode_text = entry["identity"].split(":", 1)
        mode = int(mode_text, 8)
        if kind != "dir" or mode & ~0o777:
            sys.exit("invalid source directory mode")
        identity = entry["identity"]
        restore_modes.append((path, mode))
    elif stat.S_ISREG(info.st_mode):
        h = hashlib.sha256()
        with path.open("rb") as f:
            for block in iter(lambda: f.read(1024 * 1024), b""):
                h.update(block)
        mode_text, expected_hash = entry["identity"].split(":", 1)
        identity = mode_text + ":" + h.hexdigest()
        mode = int(mode_text, 8)
        if mode & ~0o777:
            sys.exit("invalid source file mode")
        restore_modes.append((path, mode))
    else:
        sys.exit("unsupported source bundle file: " + str(relative))
    if identity != entry["identity"]:
        sys.exit("source bundle input changed: " + str(relative))
# Peipkg stores its own permissions; transport tar modes are not Unix source
# modes. Restore the recorded build-input modes only after every hash verifies.
for path, mode in restore_modes:
    os.chmod(path, mode)
command = sys.argv[1] if len(sys.argv) > 1 else "test"
if command not in ("build", "test", "package"):
    sys.exit("usage: rebuild.py [build|test|package] [additional Pekit flags]")
recipe = root / manifest["recipe"]
if not recipe.resolve().is_relative_to(root):
    sys.exit("recipe escapes source bundle")
args = ["pekit", "--recipe", str(recipe), command, "--local=" + str(root / "source")]
if manifest["source_version"]:
    args += ["--version", manifest["source_version"]]
environment = os.environ.get("PEKIT_REBUILD_ENV", manifest["environment"])
if environment:
    args += ["--env", environment]
args += sys.argv[2:]
os.execvp(args[0], args)
`
