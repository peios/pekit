package pekit

import (
	"os"
	"path/filepath"
	"sort"
)

// Additional authenticated upstream inputs.
//
// A recipe has one [source]; an input is how it names the other upstreams its
// build consumes. The Peios kernel is the case that forced them: it needs both
// its pkm tree and the pristine Linux tarball, and before inputs the only way
// to express that was to wrap the tarball in a package whose sole purpose was
// to be a dependency.
//
// An input is deliberately dull: it is a [source.url] addressed by name, put
// through the same download, the same signature verification and the same
// lockfile pinning. What differs is that there may be several, each carries
// its own version, and none of them is swept by version discovery — an input
// is pinned, and moving it is an explicit act.
//
// Inputs belong with the targets that read them. When a recipe delegates its
// build, those targets come from the fetched source tree, and so do the
// inputs they need: the source's own [input.*] blocks join the recipe's. The
// delegating recipe still resolves, verifies and locks them, so its lockfile
// remains the one trust record for everything the build consumed.

// InputState is a materialised input, as targets and the source package see it.
type InputState struct {
	Name    string
	Version string
	// Root is what PEKIT_INPUT_<NAME> points at: the extracted tree when the
	// input extracts, otherwise a directory holding the artifact.
	Root string
	// Artifact is the pristine download — exactly the bytes the lock hash
	// covers, and what corresponding-source emission carries.
	Artifact string
}

func inputBase(outBase string) string {
	return filepath.Join(outBase, "_inputs")
}

// inputDirName keys an input's cache and materialisation by version as well
// as name. Delegated sources can pin a different version at each release, and
// a tree extracted for one version must never be reused for another.
func inputDirName(cfg InputConfig) string {
	return cfg.Name + "-" + cfg.Version
}

// delegatedInputs returns the inputs a build of sourceRoot consumes: the
// recipe's own, plus — when the recipe delegates its build — those declared by
// the source tree's pekit.toml. A recipe input replaces a source input of the
// same name as a whole, the same rule that governs delegated targets.
func delegatedInputs(recipe RecipeConfig, sourceRoot string) ([]InputConfig, error) {
	if !recipe.Delegate.AllowsBuild() || sourceRoot == "" || sourceRoot == recipe.Root {
		return recipe.Inputs, nil
	}
	path := filepath.Join(sourceRoot, "pekit.toml")
	if !fileExists(path) {
		return recipe.Inputs, nil
	}
	delegated, err := LoadRecipe(path)
	if err != nil {
		return nil, err
	}
	return mergeInputs(recipe.Inputs, delegated.Inputs), nil
}

func mergeInputs(own, delegated []InputConfig) []InputConfig {
	if len(delegated) == 0 {
		return own
	}
	merged := make([]InputConfig, 0, len(own)+len(delegated))
	declared := map[string]bool{}
	for _, input := range own {
		declared[input.Name] = true
		merged = append(merged, input)
	}
	for _, input := range delegated {
		if !declared[input.Name] {
			merged = append(merged, input)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	return merged
}

// resolveInputs materialises every input. Order follows the sorted names so a
// failure is reported the same way on every run.
func resolveInputs(ctx *Context, recipe RecipeConfig, inputs []InputConfig, outBase string) ([]InputState, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	states := make([]InputState, 0, len(inputs))
	for _, cfg := range inputs {
		state, err := resolveInput(ctx, recipe, outBase, cfg)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func resolveInput(ctx *Context, recipe RecipeConfig, outBase string, cfg InputConfig) (InputState, error) {
	version, err := ParseVersion(cfg.Version)
	if err != nil {
		return InputState{}, diag("invalid_version", "input.%s: %s", cfg.Name, err.Error())
	}
	renderedURL, err := RenderTemplate(cfg.URL.URL, TemplateContext{Version: version})
	if err != nil {
		return InputState{}, wrapDiag("template", "render input."+cfg.Name+".url", err)
	}
	root, err := RenderTemplate(cfg.URL.Root, TemplateContext{Version: version})
	if err != nil {
		return InputState{}, wrapDiag("template", "render input."+cfg.Name+".root", err)
	}
	root, err = cleanRelPath(root)
	if err != nil {
		return InputState{}, wrapDiag("invalid_path", "input."+cfg.Name+".root", err)
	}

	target := filepath.Join(inputBase(outBase), inputDirName(cfg))
	if ctx.Inv.DryRun {
		// Nothing is fetched, but targets still need a stable path to render.
		return InputState{Name: cfg.Name, Version: cfg.Version, Root: target}, nil
	}

	rawDir := filepath.Join(outBase, "_source_cache", "input", inputDirName(cfg))
	artifact := filepath.Join(rawDir, urlArtifactName(renderedURL))
	// A repin must judge freshly downloaded bytes, not re-bless the cache.
	if ctx.Inv.RefreshSource || ctx.Inv.Repin {
		_ = os.RemoveAll(rawDir)
		_ = os.RemoveAll(target)
	}
	if !fileExists(artifact) {
		if err := os.MkdirAll(rawDir, 0o755); err != nil {
			return InputState{}, wrapDiag("mkdir", rawDir, err)
		}
		if err := downloadFile(renderedURL, artifact); err != nil {
			return InputState{}, wrapDiag("download", renderedURL, err)
		}
	}
	if err := applyInputLock(ctx, recipe, cfg, renderedURL, artifact, version); err != nil {
		return InputState{}, err
	}
	if err := materialiseInput(cfg, artifact, root, target); err != nil {
		return InputState{}, err
	}
	return InputState{Name: cfg.Name, Version: cfg.Version, Root: target, Artifact: artifact}, nil
}

// applyInputLock enforces the lockfile against a fetched input: a locked
// version must hash to its pin; an unlocked one is verified — signature
// first, when configured — and pinned, exactly as a new version of a url
// source is. The lock runs on cache hits too, so a poisoned cache is caught
// the same as changed upstream bytes. The pin always goes to the recipe being
// built, including for an input a delegated source declared.
func applyInputLock(ctx *Context, recipe RecipeConfig, cfg InputConfig, renderedURL, artifact string, version Version) error {
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return err
	}
	hash, err := fileSHA256(artifact)
	if err != nil {
		return wrapDiag("lock_hash", artifact, err)
	}
	entry := lock.FindInput(cfg.Name, cfg.Version)
	if entry != nil && !ctx.Inv.Repin {
		if entry.SHA256 != hash {
			return diag("lock_mismatch",
				"input %q %s hashes to sha256:%s but is locked to sha256:%s — upstream's published bytes changed; if that change is legitimate, run `pekit lock --repin`",
				cfg.Name, cfg.Version, hash, entry.SHA256)
		}
		// A configured signature must have been recorded when the pin was
		// taken; a pin from before the block was added is not evidence.
		if cfg.URL.Signature.Configured() && entry.SignatureKey == "" {
			fpr, err := verifyURLSignature(ctx, cfg.KeyRoot(), cfg.URL.Signature, renderedURL, artifact, version, "input."+cfg.Name+".signature")
			if err != nil {
				return err
			}
			entry.SignatureKey = fpr
			lock.PutInput(*entry)
			return SaveLockFile(recipe.Root, lock)
		}
		return nil
	}

	fingerprint := ""
	if cfg.URL.Signature.Configured() {
		fingerprint, err = verifyURLSignature(ctx, cfg.KeyRoot(), cfg.URL.Signature, renderedURL, artifact, version, "input."+cfg.Name+".signature")
		if err != nil {
			return err
		}
	}
	if ctx.Inv.DryRun {
		return nil
	}
	lock.PutInput(LockInput{
		Name:         cfg.Name,
		Version:      cfg.Version,
		URL:          renderedURL,
		SHA256:       hash,
		SignatureKey: fingerprint,
		LockedAt:     ctx.Start.UTC().Format("2006-01-02T15:04:05Z"),
	})
	return SaveLockFile(recipe.Root, lock)
}

// materialiseInput puts the verified artifact where targets will read it:
// extracted, with `root` naming the directory to promote, or copied whole.
func materialiseInput(cfg InputConfig, artifact, root, target string) error {
	if dirExists(target) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(target), err)
	}
	if !cfg.URL.Extract {
		staged := target + ".staging"
		_ = os.RemoveAll(staged)
		if err := os.MkdirAll(staged, 0o755); err != nil {
			return wrapDiag("mkdir", staged, err)
		}
		if err := copyFile(artifact, filepath.Join(staged, filepath.Base(artifact))); err != nil {
			return err
		}
		return promoteInput(staged, target)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".extract-*")
	if err != nil {
		return wrapDiag("mkdir", filepath.Dir(target), err)
	}
	defer os.RemoveAll(tmp)
	if err := extractArchive(artifact, tmp); err != nil {
		return err
	}
	selected := filepath.Join(tmp, filepath.FromSlash(root))
	if !dirExists(selected) {
		return diag("missing_source_root", "input %q: extracted root %s does not exist", cfg.Name, root)
	}
	return promoteInput(selected, target)
}

func promoteInput(staged, target string) error {
	if err := os.Rename(staged, target); err != nil {
		return wrapDiag("rename", "promote input", err)
	}
	return nil
}
