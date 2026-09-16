package pekit

import (
	_ "embed"
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
	PreparedArchive string             `json:"prepared_archive,omitempty"`
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
			if !sourceLinkContained(inputs.Directory, entry.Source) {
				return nil, diag("source_input_escape", "source bundle contains an unsafe link: %s", entry.Dest)
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
	entries, err = compactSourceBundle(entries, &manifest, root, stage, 90_000)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data) >= 64<<20 {
		return nil, diag("source_bundle_limit", "source identity manifest exceeds 64 MiB")
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
	readme := fmt.Sprintf("# Rebuilding %s\n\nThis schema-%d bundle contains pristine upstream archives, the prepared source tree (with patches already applied), the complete recipe tree, inherited workspace policy, declared shared inputs and build-environment records. Large prepared trees travel in prepared-source.tar and are verified and unpacked by rebuild.py. File modes, link targets and hashes are listed in build-inputs.json. Verify the outer .peipkg signature using your distribution trust configuration before trusting this bundle.\n\nRun `python3 rebuild.py test` or `python3 rebuild.py package`. Additional Pekit flags may follow; PEKIT_REBUILD_ENV selects a captured environment (default: the original profile). A compatible Pekit, Python 3, Bubblewrap and the selected profile's root-preparation tools must be installed. The native profile needs a trusted repository at workspace/_peipkgRepo_; fresh Debian acquisition needs Docker and archive access. For a catalogue profile with retained-root support, set PEKIT_DEBIAN_REPLAY to the absolute path of build-environment and PEKIT_DEBIAN_ROOT_STORE to the retained archive directory before running rebuild.py; dependency-root replay invokes neither Docker nor APT. Root archives are stored separately from this source bundle. These are declared dependency services, not missing recipe sources. Signing, when required, uses your own operator-provided keys. Production private keys are never distributed.\n\nReconstruction uses the included prepared source with --local and the recorded upstream version. It never fetches upstream or reapplies patches. Local reconstruction artifacts carry local provenance; republishing them as original signed releases is not implied. When this job ran build.vendor, acquisition/vendor contains the captured vendored outputs as well. The generic reconstruction command may still run the declared acquisition stage; these captured sources are available for offline language-specific replay. The Debian preparer verifies matching policy, architecture, requested dependencies, inventory and root archive hashes before replay, with no acquisition fallback. Keep the referenced archives for supported releases. Other providers require their own historical dependency replay contract. Bit-for-bit package reproducibility remains a separate release check; a retained root does not freeze the host kernel, clock, CPU or signing identity. New normal jobs continue automatic upstream tracking.\n", version.Raw, manifest.Schema)
	if err := os.WriteFile(readmePath, []byte(readme), 0644); err != nil {
		return nil, err
	}
	entries = append(entries, payloadEntry{Source: manifestPath, Dest: root + "/build-inputs.json"}, payloadEntry{Source: scriptPath, Dest: root + "/rebuild.py"}, payloadEntry{Source: readmePath, Dest: root + "/REBUILD.md"})
	return entries, nil
}

//go:embed source_rebuild.py
var sourceRebuildScript string
