package pekit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

type SourceState struct {
	Kind          string
	Scope         string
	OutBase       string
	WorkBase      string
	SourceRoot    string
	LiteralRoot   string
	ProvenanceRef string
	Timestamp     int64
	Local         bool
	Unanchored    bool
	// Artifact is the cached download of a url source — the pristine
	// upstream bytes, exactly what the lock hash covers. Empty for other
	// kinds and on dry runs.
	Artifact string
	// AdditionalArtifacts are ordered upstream inputs layered over Artifact,
	// currently the incremental files in a URL patch series. Corresponding-
	// source packages carry them alongside the base archive.
	AdditionalArtifacts []string
	// GitRepo and Commit identify a git source's mirror clone and the
	// resolved commit, so a consumer can re-export the exact tree (git
	// archive) without trusting the mutable checkout. Empty for other
	// kinds and on dry runs.
	GitRepo string
	Commit  string
	// TrackedPath is non-empty for a sparse tracked-path git snapshot. The
	// materialised tree and corresponding-source archive contain only this
	// path, while GitRepo and Commit still identify its immutable origin.
	TrackedPath string
}

type SourceManifest struct {
	Kind          string `json:"kind"`
	Rendered      string `json:"rendered"`
	Immutable     string `json:"immutable,omitempty"`
	Checksum      string `json:"checksum,omitempty"`
	Extract       bool   `json:"extract,omitempty"`
	Root          string `json:"root,omitempty"`
	SourceRoot    string `json:"source_root"`
	ProvenanceRef string `json:"provenance_ref"`
	Timestamp     int64  `json:"timestamp"`
	Patches       string `json:"patches,omitempty"`
}

func ResolveSource(ctx *Context, recipe RecipeConfig, version Version) (SourceState, error) {
	outBase := recipe.OutDir
	if !filepath.IsAbs(outBase) {
		outBase = filepath.Join(recipe.Root, outBase)
	}
	source := recipe.Source
	if source.Local.Path != "" && source.Local.ResolvedPath == "" {
		resolved, err := absPath(recipe.Root, source.Local.Path)
		if err != nil {
			return SourceState{}, wrapDiag("invalid_path", "resolve source.local.path", err)
		}
		source.Local.ResolvedPath = resolved
	}
	if !source.HasExternal() && (ctx.Inv.Local != nil || ctx.Inv.PreferLocal != nil) {
		if ctx.Inv.AllowUnused {
			ctx.Renderer.Event(Event{Type: "warning", Message: "local source flag ignored for sourceless recipe"})
			return sourcelessSource(recipe, outBase), nil
		}
		return SourceState{}, diag("unsupported_flag", "local source flags are invalid for sourceless recipes")
	}
	if ctx.Inv.Local != nil {
		return resolveLocalSource(ctx, recipe, outBase, source, version, true)
	}
	if ctx.Inv.PreferLocal != nil && ignorePreferLocal(ctx.Inv) {
		ctx.Renderer.Event(Event{Type: "warning", Message: "prefer-local ignored for enumerable version selector under --allow-unused"})
	} else if ctx.Inv.PreferLocal != nil {
		if st, err := resolveLocalSource(ctx, recipe, outBase, source, version, false); err == nil {
			return st, nil
		} else if !source.HasReproducible() {
			return SourceState{}, err
		}
	}
	if source.Git.URL != "" {
		if source.Git.TrackedPath != "" {
			return resolveTrackedGitSource(ctx, recipe, outBase, source.Git, version)
		}
		return resolveGitSource(ctx, recipe, outBase, source.Git, version)
	}
	if source.URL.URL != "" {
		return resolveURLSource(ctx, recipe, outBase, source.URL, version)
	}
	if source.PyPI.Project != "" {
		return resolvePyPISource(ctx, recipe, outBase, source.PyPI, version)
	}
	return sourcelessSource(recipe, outBase), nil
}

func sourcelessSource(recipe RecipeConfig, outBase string) SourceState {
	return SourceState{
		Kind:          "recipe",
		Scope:         "recipe",
		OutBase:       outBase,
		WorkBase:      outBase,
		SourceRoot:    recipe.Root,
		LiteralRoot:   recipe.Root,
		ProvenanceRef: "recipe:" + recipe.Root,
		Timestamp:     gitWorktreeTimestamp(recipe.Root),
	}
}

func resolveLocalSource(ctx *Context, recipe RecipeConfig, outBase string, source SourceConfig, version Version, strict bool) (SourceState, error) {
	path := ""
	if ctx.Inv.Local != nil && *ctx.Inv.Local != "" {
		resolved, err := absPath(ctx.Inv.Cwd, *ctx.Inv.Local)
		if err != nil {
			return SourceState{}, wrapDiag("invalid_path", "resolve --local", err)
		}
		path = resolved
	} else if ctx.Inv.PreferLocal != nil && *ctx.Inv.PreferLocal != "" {
		resolved, err := absPath(ctx.Inv.Cwd, *ctx.Inv.PreferLocal)
		if err != nil {
			return SourceState{}, wrapDiag("invalid_path", "resolve --prefer-local", err)
		}
		path = resolved
	} else {
		path = source.Local.ResolvedPath
	}
	if path == "" {
		if strict {
			return SourceState{}, diag("missing_local_source", "--local requires [source.local] or --local=<path>")
		}
		return SourceState{}, diag("missing_local_source", "preferred local source is not configured")
	}
	if !dirExists(path) {
		if strict {
			return SourceState{}, diag("missing_local_source", "local source %s is not a directory", path)
		}
		return SourceState{}, diag("missing_local_source", "preferred local source %s is not a directory", path)
	}
	if _, err := os.ReadDir(path); err != nil {
		return SourceState{}, wrapDiag("local_source_unreadable", path, err)
	}
	if source.Patches != "" {
		// A local tree is the developer's own working state — often with the
		// series already applied or mid-rework — so pekit never patches it.
		ctx.Renderer.Event(Event{Type: "warning", Message: "patches are not applied to a local source tree"})
	}
	scope := "local-" + shortHash(path)
	workBase := filepath.Join(outBase, scope)
	return SourceState{
		Kind:          "local",
		Scope:         scope,
		OutBase:       outBase,
		WorkBase:      workBase,
		SourceRoot:    path,
		LiteralRoot:   path,
		ProvenanceRef: "local:" + path,
		Timestamp:     ctx.Start.Unix(),
		Local:         true,
	}, nil
}

func resolveGitSource(ctx *Context, recipe RecipeConfig, outBase string, cfg GitSourceConfig, version Version) (SourceState, error) {
	ref, err := RenderTemplate(cfg.Ref, TemplateContext{Version: version})
	if err != nil {
		return SourceState{}, wrapDiag("template", "render source.git.ref", err)
	}
	if strings.TrimSpace(ref) == "" {
		return SourceState{}, diag("missing_version", "source.git.ref rendered empty; pass --version or set a non-templated ref")
	}
	repoKey := shortHash(cfg.URL)
	rawRepo := filepath.Join(outBase, "_source_cache", "git", repoKey, "repo.git")
	if ctx.Inv.RefreshSource {
		_ = os.RemoveAll(rawRepo)
	}
	mirrorExisted := dirExists(rawRepo)
	if !mirrorExisted {
		if ctx.Inv.DryRun {
			scope := "git-" + shortHash(cfg.URL, ref)
			return drySource("git", outBase, scope, "git:"+cfg.URL+"@"+ref), nil
		}
		if err := os.MkdirAll(filepath.Dir(rawRepo), 0o755); err != nil {
			return SourceState{}, wrapDiag("mkdir", filepath.Dir(rawRepo), err)
		}
		if err := runSimple("", "git", "clone", "--mirror", cfg.URL, rawRepo); err != nil {
			return SourceState{}, wrapDiag("git_clone", "clone source git", err)
		}
	}
	// A failed refresh is fatal only when nothing pins what the ref means.
	// A locked version whose pinned commit is already mirrored builds from
	// the lock instead: the lock asserts the exact bytes, so the remote has
	// nothing the build needs. Rate-limited upstreams (sourceware's 429s)
	// and offline builds land here; the moved-tag tripwire still fires on
	// every refresh that succeeds.
	fallbackCommit := ""
	if mirrorExisted && !ctx.Inv.DryRun {
		if err := runSimple(rawRepo, "git", "fetch", "--prune", "--tags"); err != nil {
			fallbackCommit = lockedMirroredCommit(recipe, ref, version, rawRepo)
			if fallbackCommit == "" {
				return SourceState{}, wrapDiag("git_fetch", "fetch source git", err)
			}
			ctx.Renderer.Event(Event{Type: "warning", Message: "source refresh failed (" + firstErrorLine(err) + "); building from locked commit " + fallbackCommit})
		}
	}
	commit := ref
	if fallbackCommit != "" {
		commit = fallbackCommit
	} else if !ctx.Inv.DryRun || dirExists(rawRepo) {
		out, err := commandOutput(rawRepo, "git", "rev-parse", ref+"^{commit}")
		if err != nil && !ctx.Inv.DryRun {
			return SourceState{}, wrapDiag("git_resolve", "resolve git ref "+ref, err)
		}
		if err == nil {
			commit = strings.TrimSpace(out)
		}
	}
	// Git sources lock only under a selected version: a bare branch ref is a
	// deliberately moving target, like --local.
	if !ctx.Inv.DryRun && version.Raw != "" {
		if err := applyGitLock(ctx, recipe, ref, commit, version); err != nil {
			return SourceState{}, err
		}
	}
	sourceTimestamp := gitObjectTimestamp(rawRepo, commit)
	scope := "git-" + shortHash(cfg.URL, commit)
	sourceRoot := filepath.Join(outBase, scope, "source")
	if ctx.Inv.RefreshSource {
		_ = os.RemoveAll(filepath.Join(outBase, scope))
	}
	if !ctx.Inv.DryRun {
		if !dirExists(sourceRoot) {
			if err := os.MkdirAll(filepath.Dir(sourceRoot), 0o755); err != nil {
				return SourceState{}, wrapDiag("mkdir", filepath.Dir(sourceRoot), err)
			}
			if err := runSimple("", "git", "clone", rawRepo, sourceRoot); err != nil {
				return SourceState{}, wrapDiag("git_checkout", "prepare source checkout", err)
			}
		}
		if err := runSimple(sourceRoot, "git", "reset", "--hard", commit); err != nil {
			return SourceState{}, wrapDiag("git_reset", "reset source checkout", err)
		}
		if err := runSimple(sourceRoot, "git", "clean", "-fdx"); err != nil {
			return SourceState{}, wrapDiag("git_clean", "clean source checkout", err)
		}
		// Reset + clean restored the pristine tree, so the series re-applies
		// on every resolve; an edited patch takes effect without a scope
		// change.
		ps, err := loadPatchSet(recipe, ctx.Inv.AllowUnused)
		if err != nil {
			return SourceState{}, err
		}
		if err := applyPatchSet(ctx, ps, sourceRoot, version); err != nil {
			return SourceState{}, err
		}
		patchesHash := ""
		if ps != nil {
			patchesHash = ps.Hash
		}
		if err := writeSourceManifest(filepath.Join(outBase, scope, "source.pekit.json"), SourceManifest{
			Kind:          "git",
			Rendered:      cfg.URL + "@" + ref,
			Immutable:     commit,
			SourceRoot:    sourceRoot,
			ProvenanceRef: "git:" + cfg.URL + "@" + commit,
			Timestamp:     sourceTimestamp,
			Patches:       patchesHash,
		}); err != nil {
			return SourceState{}, err
		}
	}
	return SourceState{
		Kind:          "git",
		Scope:         scope,
		OutBase:       outBase,
		WorkBase:      filepath.Join(outBase, scope),
		SourceRoot:    sourceRoot,
		LiteralRoot:   sourceRoot,
		ProvenanceRef: "git:" + cfg.URL + "@" + commit,
		Timestamp:     sourceTimestamp,
		GitRepo:       rawRepo,
		Commit:        commit,
	}, nil
}

func resolveURLSource(ctx *Context, recipe RecipeConfig, outBase string, cfg URLSourceConfig, version Version) (SourceState, error) {
	baseVersion, err := urlBaseVersion(cfg, version)
	if err != nil {
		return SourceState{}, err
	}
	renderedURL, err := RenderTemplate(cfg.URL, TemplateContext{Version: baseVersion})
	if err != nil {
		return SourceState{}, wrapDiag("template", "render source.url.url", err)
	}
	root, err := RenderTemplate(cfg.Root, TemplateContext{Version: baseVersion})
	if err != nil {
		return SourceState{}, wrapDiag("template", "render source.url.root", err)
	}
	root, err = cleanRelPath(root)
	if err != nil {
		return SourceState{}, wrapDiag("invalid_path", "source.url.root", err)
	}
	checksum := cfg.Checksum
	if baseVersion.Raw != "" && len(cfg.ChecksumByVersion) > 0 {
		var ok bool
		checksum, ok = cfg.ChecksumByVersion[baseVersion.Raw]
		if !ok {
			return SourceState{}, diag("missing_checksum", "source.url.checksum has no entry for base version %s", baseVersion.Raw)
		}
	}
	ps, err := loadPatchSet(recipe, ctx.Inv.AllowUnused)
	if err != nil {
		return SourceState{}, err
	}
	// Unlike a git checkout there is no pristine state to reset to, so a
	// patched url tree is materialised once per patch-set content: the
	// series hash joins the scope and an edited patch lands in a fresh
	// extraction. Unpatched recipes keep their existing scopes.
	scopeParts := []string{renderedURL, checksum, root}
	if cfg.PatchSeries.Configured() {
		scopeParts = append(scopeParts, cfg.PatchSeries.URL, version.Raw, fmt.Sprintf("strip=%d", cfg.PatchSeries.Strip))
	}
	if ps != nil && ps.Hash != "" {
		scopeParts = append(scopeParts, ps.Hash)
	}
	scope := "url-" + shortHash(scopeParts...)
	if ctx.Inv.DryRun {
		provenance := "url:" + renderedURL
		if checksum != "" {
			provenance += "#" + checksum
		}
		st := drySource("url", outBase, scope, provenance)
		st.Unanchored = checksum == ""
		// A dry run fetches nothing, but an existing lock entry still anchors
		// the version.
		if st.Unanchored {
			if lock, err := LoadLockFile(recipe.Root); err == nil {
				if e := lock.Find(version.Raw); e != nil && e.SHA256 != "" {
					st.Unanchored = false
				}
			}
		}
		return st, nil
	}
	rawDir := filepath.Join(outBase, "_source_cache", "url", shortHash(renderedURL))
	artifact := filepath.Join(rawDir, urlArtifactName(renderedURL))
	// A repin must judge freshly downloaded bytes — its purpose is accepting
	// what upstream publishes now, not re-blessing the cache.
	if ctx.Inv.RefreshSource || ctx.Inv.Repin {
		_ = os.RemoveAll(rawDir)
		_ = os.RemoveAll(filepath.Join(outBase, scope))
	}
	if !fileExists(artifact) {
		legacyArtifact := filepath.Join(rawDir, "artifact")
		if legacyArtifact != artifact && fileExists(legacyArtifact) {
			if err := os.Rename(legacyArtifact, artifact); err != nil {
				return SourceState{}, wrapDiag("rename", "preserve URL artifact extension", err)
			}
		}
	}
	if !fileExists(artifact) {
		if err := os.MkdirAll(rawDir, 0o755); err != nil {
			return SourceState{}, wrapDiag("mkdir", rawDir, err)
		}
		if err := downloadFile(renderedURL, artifact); err != nil {
			return SourceState{}, wrapDiag("download", renderedURL, err)
		}
	}
	if checksum != "" {
		if err := verifyChecksum(artifact, checksum); err != nil {
			_ = os.Remove(artifact)
			if err2 := downloadFile(renderedURL, artifact); err2 != nil {
				return SourceState{}, wrapDiag("download", renderedURL, err2)
			}
			if err2 := verifyChecksum(artifact, checksum); err2 != nil {
				return SourceState{}, wrapDiag("checksum", renderedURL, err2)
			}
		}
	}
	remotePatches, err := fetchURLPatchArtifacts(ctx, outBase, cfg.PatchSeries, version)
	if err != nil {
		return SourceState{}, err
	}
	// The lock runs on every resolve, cache hits included, so a poisoned
	// cache entry is caught the same as a changed upstream.
	lockState, err := applyURLLockWithPatches(ctx, recipe, cfg, renderedURL, artifact, version, baseVersion, remotePatches)
	if err != nil {
		return SourceState{}, err
	}
	sourceRoot := filepath.Join(outBase, scope, "source")
	manifestPath := filepath.Join(outBase, scope, "source.pekit.json")
	provenance := "url:" + renderedURL
	unanchored := checksum == "" && !lockState.Locked
	sourceTimestamp := int64(0)
	if !unanchored {
		sourceTimestamp = gitWorktreeTimestamp(recipe.Root)
	}
	if checksum != "" {
		provenance += "#" + checksum
	} else if lockState.Locked {
		provenance += "#sha256:" + lockState.Hash
	}
	if lockState.PatchHash != "" {
		provenance += "+patches:sha256:" + lockState.PatchHash
	}
	patchesHash := ""
	if ps != nil {
		patchesHash = ps.Hash
	}
	if lockState.PatchHash != "" {
		patchesHash = lockState.PatchHash + ":" + patchesHash
	}
	expectedManifest := SourceManifest{
		Kind:          "url",
		Rendered:      renderedURL,
		Checksum:      checksum,
		Extract:       cfg.Extract,
		Root:          root,
		SourceRoot:    sourceRoot,
		ProvenanceRef: provenance,
		Timestamp:     sourceTimestamp,
		Patches:       patchesHash,
	}
	if checksum != "" && dirExists(sourceRoot) {
		if err := os.RemoveAll(filepath.Join(outBase, scope)); err != nil {
			return SourceState{}, wrapDiag("clean_source", filepath.Join(outBase, scope), err)
		}
	}
	if dirExists(sourceRoot) && !sourceManifestMatches(manifestPath, expectedManifest) {
		if err := os.RemoveAll(filepath.Join(outBase, scope)); err != nil {
			return SourceState{}, wrapDiag("clean_source", filepath.Join(outBase, scope), err)
		}
	}
	if !dirExists(sourceRoot) {
		if err := os.MkdirAll(filepath.Dir(sourceRoot), 0o755); err != nil {
			return SourceState{}, wrapDiag("mkdir", filepath.Dir(sourceRoot), err)
		}
		if cfg.Extract {
			tmp, err := os.MkdirTemp(filepath.Dir(sourceRoot), ".extract-*")
			if err != nil {
				return SourceState{}, wrapDiag("mkdir", filepath.Dir(sourceRoot), err)
			}
			defer os.RemoveAll(tmp)
			if err := extractArchive(artifact, tmp); err != nil {
				return SourceState{}, err
			}
			selected := filepath.Join(tmp, filepath.FromSlash(root))
			if !dirExists(selected) {
				return SourceState{}, diag("missing_source_root", "extracted root %s does not exist", root)
			}
			if err := os.Rename(selected, sourceRoot); err != nil {
				return SourceState{}, wrapDiag("rename", "promote extracted source", err)
			}
		} else {
			if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
				return SourceState{}, wrapDiag("mkdir", sourceRoot, err)
			}
			if err := copyFile(artifact, filepath.Join(sourceRoot, filepath.Base(artifact))); err != nil {
				return SourceState{}, err
			}
		}
		if err := applyURLPatchArtifacts(ctx, remotePatches, sourceRoot, version); err != nil {
			_ = os.RemoveAll(filepath.Join(outBase, scope))
			return SourceState{}, err
		}
		if err := applyPatchSet(ctx, ps, sourceRoot, version); err != nil {
			// Never leave a half-patched tree that a rerun would take for a
			// cached materialisation.
			_ = os.RemoveAll(filepath.Join(outBase, scope))
			return SourceState{}, err
		}
		if err := writeSourceManifest(manifestPath, expectedManifest); err != nil {
			return SourceState{}, err
		}
	}
	additionalArtifacts := make([]string, 0, len(remotePatches))
	for _, patch := range remotePatches {
		additionalArtifacts = append(additionalArtifacts, patch.Artifact)
	}
	return SourceState{
		Kind:                "url",
		Scope:               scope,
		OutBase:             outBase,
		WorkBase:            filepath.Join(outBase, scope),
		SourceRoot:          sourceRoot,
		LiteralRoot:         sourceRoot,
		ProvenanceRef:       provenance,
		Timestamp:           sourceTimestamp,
		Unanchored:          unanchored,
		Artifact:            artifact,
		AdditionalArtifacts: additionalArtifacts,
	}, nil
}

func urlBaseVersion(cfg URLSourceConfig, selected Version) (Version, error) {
	if !cfg.PatchSeries.Configured() {
		return selected, nil
	}
	if !selected.Parsed || selected.Minor == "" || selected.Patch == "" || selected.Prerelease != "" || selected.BuildMeta != "" {
		return Version{}, diag("invalid_patch_series_version", "source.url.patch_series requires a stable major.minor.patch version, got %q", selected.Raw)
	}
	base := selected
	base.Raw = selected.Major + "." + selected.Minor
	base.Patch = ""
	return base, nil
}

func fetchURLPatchArtifacts(ctx *Context, outBase string, cfg URLPatchSeriesConfig, version Version) ([]urlPatchArtifact, error) {
	if !cfg.Configured() {
		return nil, nil
	}
	if !version.Parsed || version.Minor == "" || version.Patch == "" || version.Prerelease != "" || version.BuildMeta != "" {
		return nil, diag("invalid_patch_series_version", "source.url.patch_series requires a stable major.minor.patch version, got %q", version.Raw)
	}
	level := 0
	if version.Patch != "" {
		var err error
		level, err = strconv.Atoi(version.Patch)
		if err != nil || level < 0 {
			return nil, diag("invalid_patch_series_version", "source.url.patch_series requires a numeric patch component, got %q", version.Raw)
		}
	}
	patches := make([]urlPatchArtifact, 0, level)
	for current := 1; current <= level; current++ {
		rendered, err := renderURLPatch(cfg, version, current)
		if err != nil {
			return nil, err
		}
		rawDir := filepath.Join(outBase, "_source_cache", "url", shortHash(rendered))
		artifact := filepath.Join(rawDir, urlArtifactName(rendered))
		if ctx.Inv.RefreshSource || ctx.Inv.Repin {
			_ = os.RemoveAll(rawDir)
		}
		if !fileExists(artifact) {
			if err := os.MkdirAll(rawDir, 0o755); err != nil {
				return nil, wrapDiag("mkdir", rawDir, err)
			}
			if err := downloadFile(rendered, artifact); err != nil {
				return nil, wrapDiag("download", rendered, err)
			}
		}
		patches = append(patches, urlPatchArtifact{URL: rendered, Artifact: artifact, Config: cfg, Version: urlPatchVersion(cfg, version, current)})
	}
	return patches, nil
}

func applyURLPatchArtifacts(ctx *Context, patches []urlPatchArtifact, tree string, version Version) error {
	for i, patchArtifact := range patches {
		strip := fmt.Sprintf("-p%d", patchArtifact.Config.Strip)
		if err := runSimple(tree, "patch", "--batch", "--forward", "--fuzz=0", "--no-backup-if-mismatch", strip, "-i", patchArtifact.Artifact); err != nil {
			return wrapDiag("patch_apply", fmt.Sprintf("apply upstream patch %d (%s)", i+1, patchArtifact.URL), err)
		}
	}
	if len(patches) > 0 {
		ctx.Renderer.Event(Event{Type: "patch", Version: version.Raw, Path: tree, Message: fmt.Sprintf("applied %d upstream patches", len(patches))})
	}
	return nil
}

func urlArtifactName(raw string) string {
	parsed, err := url.Parse(raw)
	name := ""
	if err == nil {
		name = path.Base(parsed.Path)
	}
	if name == "" || name == "." || name == "/" || strings.ContainsRune(name, '\x00') {
		return "artifact"
	}
	return name
}

func drySource(kind, outBase, scope, provenance string) SourceState {
	sourceRoot := filepath.Join(outBase, scope, "source")
	return SourceState{Kind: kind, Scope: scope, OutBase: outBase, WorkBase: filepath.Join(outBase, scope), SourceRoot: sourceRoot, LiteralRoot: sourceRoot, ProvenanceRef: provenance}
}

func downloadFile(url, dst string) error {
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pekit/2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dst)
}

func verifyChecksum(path, spec string) error {
	algo, want, ok := strings.Cut(spec, ":")
	if !ok || algo != "sha256" || want == "" {
		return fmt.Errorf("unsupported checksum spec %q", spec)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

func extractArchive(artifact, dst string) error {
	lower := strings.ToLower(filepath.Base(artifact))
	if strings.HasSuffix(lower, ".zip") {
		return extractZip(artifact, dst)
	}
	return extractTar(artifact, dst)
}

func extractZip(artifact, dst string) error {
	reader, err := zip.OpenReader(artifact)
	if err != nil {
		return wrapDiag("extract", artifact, err)
	}
	defer reader.Close()
	seen := map[string]string{}
	for _, file := range reader.File {
		rel, err := cleanArchiveEntryPath(file.Name)
		if err != nil {
			return wrapDiag("unsafe_archive", file.Name, err)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		kind := archiveEntryKind(file.FileInfo().Mode(), file.FileInfo().IsDir())
		if err := checkArchiveCollision(seen, rel, kind); err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := makeArchiveDir(dst, rel, file.Mode()); err != nil {
				return err
			}
			seen[rel] = "dir"
			continue
		}
		if file.Mode()&os.ModeSymlink != 0 {
			if err := ensureArchiveParentSafe(dst, rel); err != nil {
				return err
			}
			rc, err := file.Open()
			if err != nil {
				return wrapDiag("extract", file.Name, err)
			}
			targetBytes, readErr := io.ReadAll(io.LimitReader(rc, 1<<20))
			closeErr := rc.Close()
			if readErr != nil {
				return wrapDiag("extract", file.Name, readErr)
			}
			if closeErr != nil {
				return wrapDiag("extract", file.Name, closeErr)
			}
			linkTarget := string(targetBytes)
			if err := validateArchiveSymlinkTarget(rel, linkTarget); err != nil {
				return wrapDiag("unsafe_archive", file.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return wrapDiag("extract", filepath.Dir(target), err)
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return wrapDiag("extract", target, err)
			}
			seen[rel] = "symlink"
			continue
		}
		if !file.FileInfo().Mode().IsRegular() {
			return diag("unsafe_archive", "archive entry %s has unsupported file type", file.Name)
		}
		if err := ensureArchiveParentSafe(dst, rel); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return wrapDiag("extract", filepath.Dir(target), err)
		}
		rc, err := file.Open()
		if err != nil {
			return wrapDiag("extract", file.Name, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.Mode())
		if err != nil {
			_ = rc.Close()
			return wrapDiag("extract", target, err)
		}
		_, copyErr := io.Copy(out, rc)
		closeOutErr := out.Close()
		closeInErr := rc.Close()
		if copyErr != nil {
			return wrapDiag("extract", target, copyErr)
		}
		if closeOutErr != nil {
			return wrapDiag("extract", target, closeOutErr)
		}
		if closeInErr != nil {
			return wrapDiag("extract", file.Name, closeInErr)
		}
		// Same mtime preservation as writeTarFileEntry, for the same
		// maintainer-rebuild-rule reason.
		if mod := file.Modified; !mod.IsZero() {
			if err := os.Chtimes(target, mod, mod); err != nil {
				return wrapDiag("extract", target, err)
			}
		}
		seen[rel] = "file"
	}
	return nil
}

func extractTar(artifact, dst string) error {
	stream, err := openTarStream(artifact)
	if err != nil {
		return err
	}
	defer stream.Close()
	reader := tar.NewReader(stream)
	seen := map[string]string{}
	for {
		hdr, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return wrapDiag("extract", artifact, err)
		}
		// git-archive tarballs (kernel.org releases among them) open with
		// a pax global header recording the source commit. It is stream
		// metadata, not a member; per-file 'x' records are already
		// consumed transparently by archive/tar.
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel, err := cleanArchiveEntryPath(hdr.Name)
		if err != nil {
			return wrapDiag("unsafe_archive", hdr.Name, err)
		}
		kind, err := tarEntryKind(hdr)
		if err != nil {
			return err
		}
		if err := checkArchiveCollision(seen, rel, kind); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := makeArchiveDir(dst, rel, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
			seen[rel] = "dir"
		case tar.TypeReg, tar.TypeRegA:
			if err := writeTarFileEntry(dst, rel, os.FileMode(hdr.Mode), hdr.ModTime, reader); err != nil {
				return err
			}
			seen[rel] = "file"
		case tar.TypeSymlink:
			if err := writeTarSymlinkEntry(dst, rel, hdr.Linkname); err != nil {
				return err
			}
			seen[rel] = "symlink"
		}
	}
}

type archiveReadCloser struct {
	io.Reader
	close func() error
}

func (r archiveReadCloser) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}

func openTarStream(path string) (io.ReadCloser, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, wrapDiag("open", path, err)
	}
	lower := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasSuffix(lower, ".tar"):
		return file, nil
	// .crate is cargo's publish format: a plain gzipped tarball by definition
	// (crates.io serves bindgen-cli-0.65.1.crate etc.).
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"), strings.HasSuffix(lower, ".crate"):
		gr, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			return nil, wrapDiag("extract", path, err)
		}
		return archiveReadCloser{Reader: gr, close: func() error {
			err1 := gr.Close()
			err2 := file.Close()
			if err1 != nil {
				return err1
			}
			return err2
		}}, nil
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return archiveReadCloser{Reader: bzip2.NewReader(file), close: file.Close}, nil
	case strings.HasSuffix(lower, ".tar.zst"):
		zr, err := zstd.NewReader(file)
		if err != nil {
			_ = file.Close()
			return nil, wrapDiag("extract", path, err)
		}
		return archiveReadCloser{Reader: zr, close: func() error {
			zr.Close()
			return file.Close()
		}}, nil
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		_ = file.Close()
		return openExternalCompressedTar(path, "xz", "-dc", path)
	default:
		_ = file.Close()
		return nil, diag("unsupported_archive", "unsupported archive format %s", filepath.Base(path))
	}
}

func openExternalCompressedTar(path, name string, args ...string) (io.ReadCloser, error) {
	cmd := execCommand(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, wrapDiag("extract", path, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, wrapDiag("extract", path, err)
	}
	return archiveReadCloser{Reader: stdout, close: func() error {
		// Close the pipe before waiting: an early Close — extraction
		// aborted before EOF — must not deadlock on a decompressor
		// that is itself blocked writing to us. EPIPE tells it to
		// quit; at stream EOF the close is a no-op.
		_ = stdout.Close()
		err := cmd.Wait()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
		}
		return err
	}}, nil
}

func tarEntryKind(hdr *tar.Header) (string, error) {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return "dir", nil
	case tar.TypeReg, tar.TypeRegA:
		return "file", nil
	case tar.TypeSymlink:
		return "symlink", nil
	default:
		return "", diag("unsafe_archive", "archive entry %s has unsupported type %d", hdr.Name, hdr.Typeflag)
	}
}

func archiveEntryKind(mode os.FileMode, isDir bool) string {
	if isDir {
		return "dir"
	}
	if mode&os.ModeSymlink != 0 {
		return "symlink"
	}
	return "file"
}

func checkArchiveCollision(seen map[string]string, rel, kind string) error {
	if prev, ok := seen[rel]; ok {
		if prev == "dir" && kind == "dir" {
			return nil
		}
		return diag("unsafe_archive", "archive entry %s collides with prior %s entry", rel, prev)
	}
	return nil
}

func makeArchiveDir(root, rel string, mode os.FileMode) error {
	if err := ensureArchiveParentSafe(root, rel); err != nil {
		return err
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return diag("unsafe_archive", "archive directory %s would replace a symlink", rel)
		}
		if !info.IsDir() {
			return diag("unsafe_archive", "archive directory %s collides with a file", rel)
		}
		return nil
	}
	if err := os.MkdirAll(target, mode.Perm()); err != nil {
		return wrapDiag("extract", target, err)
	}
	return nil
}

func writeTarFileEntry(root, rel string, mode os.FileMode, modTime time.Time, reader io.Reader) error {
	if err := ensureArchiveParentSafe(root, rel); err != nil {
		return err
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return wrapDiag("extract", filepath.Dir(target), err)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return wrapDiag("extract", target, err)
	}
	_, copyErr := io.Copy(out, reader)
	closeErr := out.Close()
	if copyErr != nil {
		return wrapDiag("extract", target, copyErr)
	}
	if closeErr != nil {
		return wrapDiag("extract", target, closeErr)
	}
	// Preserve the archived mtime: autotools release tarballs encode
	// "generated outputs are newer than their inputs" in timestamps, and
	// write-order mtimes make the maintainer rebuild rules (autoconf,
	// automake, makeinfo) fire in environments that deliberately lack
	// those tools. Directories and symlinks don't feed make's dependency
	// checks, so only regular files need this.
	if !modTime.IsZero() {
		if err := os.Chtimes(target, modTime, modTime); err != nil {
			return wrapDiag("extract", target, err)
		}
	}
	return nil
}

func writeTarSymlinkEntry(root, rel, target string) error {
	if err := validateArchiveSymlinkTarget(rel, target); err != nil {
		return wrapDiag("unsafe_archive", rel, err)
	}
	if err := ensureArchiveParentSafe(root, rel); err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapDiag("extract", filepath.Dir(path), err)
	}
	if err := os.Symlink(target, path); err != nil {
		return wrapDiag("extract", path, err)
	}
	return nil
}

func cleanArchiveEntryPath(name string) (string, error) {
	if strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("archive entry contains NUL")
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(cleaned) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes extraction root")
	}
	return filepath.ToSlash(cleaned), nil
}

func validateArchiveSymlinkTarget(entry, target string) error {
	if target == "" {
		return fmt.Errorf("symlink target is empty")
	}
	if strings.ContainsRune(target, '\x00') {
		return fmt.Errorf("symlink target contains NUL")
	}
	if filepath.IsAbs(filepath.FromSlash(target)) {
		return fmt.Errorf("symlink target escapes extraction root")
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(entry)), filepath.FromSlash(target)))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink target escapes extraction root")
	}
	return nil
}

func ensureArchiveParentSafe(root, rel string) error {
	dir := filepath.Dir(filepath.FromSlash(rel))
	if dir == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return diag("unsafe_archive", "archive entry %s would be extracted through symlink component %s", rel, part)
		}
		if err == nil && !info.IsDir() {
			return diag("unsafe_archive", "archive entry %s parent component %s is not a directory", rel, part)
		}
		if err != nil && !os.IsNotExist(err) {
			return wrapDiag("extract", cur, err)
		}
	}
	return nil
}

func sourceManifestMatches(path string, expected SourceManifest) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var actual SourceManifest
	if err := json.Unmarshal(data, &actual); err != nil {
		return false
	}
	return actual.Kind == expected.Kind &&
		actual.Rendered == expected.Rendered &&
		actual.Immutable == expected.Immutable &&
		actual.Checksum == expected.Checksum &&
		actual.Extract == expected.Extract &&
		actual.Root == expected.Root &&
		actual.ProvenanceRef == expected.ProvenanceRef &&
		actual.Timestamp == expected.Timestamp &&
		actual.Patches == expected.Patches
}

func writeSourceManifest(path string, manifest SourceManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return wrapDiag("source_manifest", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return wrapDiag("source_manifest", path, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return wrapDiag("copy", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(dst), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return wrapDiag("copy", dst, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_, copyErr := io.Copy(tmp, in)
	closeErr := tmp.Close()
	if copyErr != nil {
		return wrapDiag("copy", dst, copyErr)
	}
	if closeErr != nil {
		return wrapDiag("copy", dst, closeErr)
	}
	info, err := os.Stat(src)
	if err == nil {
		_ = os.Chmod(tmpPath, info.Mode())
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return wrapDiag("copy", dst, err)
	}
	return nil
}

func commandOutput(dir, name string, args ...string) (string, error) {
	cmd := execCommand(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitObjectTimestamp(repoDir, ref string) int64 {
	if repoDir == "" || ref == "" || !dirExists(repoDir) {
		return 0
	}
	out, err := commandOutput(repoDir, "git", "show", "-s", "--format=%ct", ref)
	if err != nil {
		return 0
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

func gitWorktreeTimestamp(root string) int64 {
	if root == "" || !pathExists(filepath.Join(root, ".git")) {
		return 0
	}
	out, err := commandOutput(root, "git", "log", "-1", "--format=%ct")
	if err != nil {
		return 0
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
