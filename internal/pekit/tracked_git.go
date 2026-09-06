package pekit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// trackedGitSnapshot is one observation of a fixed moving ref's tracked file.
// It is cached only for the life of an invocation, bridging version discovery
// and source resolution without resolving the moving ref a second time.
type trackedGitSnapshot struct {
	Version    string
	Repository string
	Ref        string
	Path       string
	Repo       string
	Commit     string
	Blob       string
	BlobSHA256 string
	Bytes      []byte
	Mode       os.FileMode
	Timestamp  int64
}

var trackedSnapshotVersionRE = regexp.MustCompile(`^([0-9]{4}\.[0-9]{2}\.[0-9]{2})(?:\.([2-9][0-9]*))?$`)
var trackedGitObjectRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func trackedGitKey(recipe RecipeConfig, cfg GitSourceConfig) string {
	return shortHash(recipe.Root, cfg.URL, cfg.Ref, cfg.TrackedPath)
}

func trackedGitOutBase(recipe RecipeConfig) string {
	out := recipe.OutDir
	if !filepath.IsAbs(out) {
		out = filepath.Join(recipe.Root, out)
	}
	return out
}

func trackedGitRepo(recipe RecipeConfig, cfg GitSourceConfig) string {
	return filepath.Join(trackedGitOutBase(recipe), "_source_cache", "git-tracked", shortHash(cfg.URL, cfg.Ref, cfg.TrackedPath), "repo.git")
}

func enumerateTrackedGitVersions(ctx *Context, recipe RecipeConfig, cfg GitSourceConfig) ([]string, error) {
	lock, history, err := trackedGitHistory(recipe, cfg)
	if err != nil {
		return nil, err
	}
	snapshot, err := discoverTrackedGitSnapshot(ctx, recipe, cfg, lock, history)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(history)+1)
	for _, entry := range history {
		seen[entry.Version] = true
	}
	if snapshot.Version != "" {
		seen[snapshot.Version] = true
		if ctx.TrackedGit == nil {
			ctx.TrackedGit = map[string]trackedGitSnapshot{}
		}
		ctx.TrackedGit[trackedGitKey(recipe, cfg)+"\x00"+snapshot.Version] = snapshot
	}
	return sortedVersions(seen), nil
}

func trackedGitHistory(recipe RecipeConfig, cfg GitSourceConfig) (LockFile, []LockSource, error) {
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return LockFile{}, nil, err
	}
	var history []LockSource
	for _, entry := range lock.Sources {
		if !entry.isTrackedGit() {
			continue
		}
		if entry.Repository != cfg.URL || entry.Ref != cfg.Ref || entry.Path != cfg.TrackedPath {
			continue
		}
		if err := validateTrackedGitLock(entry, cfg, entry.Version); err != nil {
			return LockFile{}, nil, err
		}
		history = append(history, entry)
	}
	return lock, history, nil
}

func discoverTrackedGitSnapshot(ctx *Context, recipe RecipeConfig, cfg GitSourceConfig, lock LockFile, history []LockSource) (trackedGitSnapshot, error) {
	repo := trackedGitRepo(recipe, cfg)
	if ctx.Inv.DryRun {
		tmp, err := os.MkdirTemp("", "pekit-git-tracked-*")
		if err != nil {
			return trackedGitSnapshot{}, wrapDiag("mkdir", "create tracked git discovery cache", err)
		}
		defer os.RemoveAll(tmp)
		repo = filepath.Join(tmp, "repo.git")
	}
	if ctx.Inv.RefreshSource {
		if err := os.RemoveAll(repo); err != nil {
			return trackedGitSnapshot{}, wrapDiag("clean_source", repo, err)
		}
	}
	if err := ensureTrackedGitRepository(repo, cfg.URL); err != nil {
		return trackedGitSnapshot{}, err
	}
	if err := runSimple(repo, "git", "-c", "protocol.version=2", "fetch", "--depth=1", "--filter=blob:none", "--no-tags", "--force", "origin", cfg.Ref); err != nil {
		return trackedGitSnapshot{}, wrapDiag("git_fetch", "fetch tracked git ref "+cfg.Ref, err)
	}
	commit, err := gitRevParse(repo, "FETCH_HEAD^{commit}")
	if err != nil {
		return trackedGitSnapshot{}, wrapDiag("git_resolve", "resolve tracked git ref "+cfg.Ref, err)
	}
	snapshot, err := inspectTrackedGitObject(repo, cfg, commit)
	if err != nil {
		return trackedGitSnapshot{}, err
	}
	if len(history) > 0 {
		// History is stable-sorted on disk, but compare explicitly because a
		// hand-produced pre-existing lock need not be ordered.
		latest := history[0]
		for _, candidate := range history[1:] {
			if compareVersionText(candidate.Version, latest.Version) > 0 {
				latest = candidate
			}
		}
		if latest.BlobSHA256 == snapshot.BlobSHA256 {
			// The branch may advance for unrelated files. That is deliberately
			// not a new package version and does not rewrite old provenance.
			return trackedGitSnapshot{}, nil
		}
	}
	snapshot.Version, err = nextTrackedSnapshotVersion(ctx, history)
	if err != nil {
		return trackedGitSnapshot{}, err
	}
	if existing := lock.Find(snapshot.Version); existing != nil {
		return trackedGitSnapshot{}, diag("lock_mismatch", "tracked git snapshot version %q is already locked for another source; adjust the recipe lock deliberately rather than overwriting it", snapshot.Version)
	}
	return snapshot, nil
}

func nextTrackedSnapshotVersion(ctx *Context, history []LockSource) (string, error) {
	now := ctx.Start
	if now.IsZero() && ctx.App != nil && ctx.App.Now != nil {
		now = ctx.App.Now()
	}
	if now.IsZero() {
		now = time.Now()
	}
	date := now.UTC().Format("2006.01.02")
	latestDate := ""
	ordinals := map[string]*big.Int{}
	for _, entry := range history {
		m := trackedSnapshotVersionRE.FindStringSubmatch(entry.Version)
		if m == nil {
			return "", diag("lock_version", "tracked git lock version %q is not YYYY.MM.DD or YYYY.MM.DD.N", entry.Version)
		}
		ordinal := big.NewInt(1)
		if m[2] != "" {
			ordinal = new(big.Int)
			if _, ok := ordinal.SetString(m[2], 10); !ok {
				return "", diag("lock_version", "tracked git lock version %q has an invalid suffix", entry.Version)
			}
		}
		if current := ordinals[m[1]]; current == nil || ordinal.Cmp(current) > 0 {
			ordinals[m[1]] = new(big.Int).Set(ordinal)
		}
		if latestDate == "" || compareVersionText(m[1], latestDate) > 0 {
			latestDate = m[1]
		}
	}
	if latestDate != "" && compareVersionText(date, latestDate) < 0 {
		date = latestDate
	}
	if ordinals[date] == nil {
		return date, nil
	}
	next := new(big.Int).Add(ordinals[date], big.NewInt(1))
	return fmt.Sprintf("%s.%s", date, next.String()), nil
}

func ensureTrackedGitRepository(repo, repository string) error {
	if !dirExists(repo) {
		if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
			return wrapDiag("mkdir", filepath.Dir(repo), err)
		}
		if err := runSimple("", "git", "init", "--bare", repo); err != nil {
			return wrapDiag("git_clone", "initialise tracked git cache", err)
		}
		if err := runSimple(repo, "git", "remote", "add", "origin", repository); err != nil {
			return wrapDiag("git_remote", "configure tracked git cache", err)
		}
	} else {
		current, err := commandOutput(repo, "git", "remote", "get-url", "origin")
		if err != nil {
			return wrapDiag("git_remote", "read tracked git cache remote", err)
		}
		if strings.TrimSpace(current) != repository {
			return diag("git_remote_mismatch", "tracked git cache %s points at %q, expected %q; use --refresh-source to recreate it", repo, strings.TrimSpace(current), repository)
		}
	}
	// Mark origin as a partial-clone promisor. A server that understands the
	// filter transfers trees but fetches only the selected blob on demand.
	if err := runSimple(repo, "git", "config", "remote.origin.promisor", "true"); err != nil {
		return wrapDiag("git_remote", "configure tracked git promisor", err)
	}
	if err := runSimple(repo, "git", "config", "remote.origin.partialclonefilter", "blob:none"); err != nil {
		return wrapDiag("git_remote", "configure tracked git filter", err)
	}
	return nil
}

func ensureTrackedGitCommit(repo string, cfg GitSourceConfig, commit string) error {
	if err := runSimple(repo, "git", "cat-file", "-e", commit+"^{commit}"); err == nil {
		return nil
	}
	// Fetch the immutable object id, never the configured moving ref. If an
	// upstream refuses direct object wants, a cold locked build fails closed;
	// a populated cache remains fully offline-replayable.
	if err := runSimple(repo, "git", "-c", "protocol.version=2", "fetch", "--depth=1", "--filter=blob:none", "--no-tags", "origin", commit); err != nil {
		return wrapDiag("git_fetch", "fetch locked tracked git commit "+commit, err)
	}
	if err := runSimple(repo, "git", "cat-file", "-e", commit+"^{commit}"); err != nil {
		return wrapDiag("git_resolve", "verify locked tracked git commit "+commit, err)
	}
	return nil
}

func gitRevParse(repo, object string) (string, error) {
	out, err := commandOutput(repo, "git", "rev-parse", object)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func inspectTrackedGitObject(repo string, cfg GitSourceConfig, commit string) (trackedGitSnapshot, error) {
	blob, err := gitRevParse(repo, commit+":"+cfg.TrackedPath)
	if err != nil {
		return trackedGitSnapshot{}, wrapDiag("git_tracked_path", "resolve tracked path "+cfg.TrackedPath+" at "+commit, err)
	}
	typeName, err := commandOutput(repo, "git", "cat-file", "-t", blob)
	if err != nil || strings.TrimSpace(typeName) != "blob" {
		return trackedGitSnapshot{}, diag("git_tracked_path", "tracked path %q at %s is not a regular blob", cfg.TrackedPath, commit)
	}
	cmd := execCommand("git", "cat-file", "blob", blob)
	cmd.Dir = repo
	data, err := cmd.Output()
	if err != nil {
		return trackedGitSnapshot{}, wrapDiag("git_tracked_path", "read tracked blob "+blob, err)
	}
	ls, err := commandOutput(repo, "git", "ls-tree", "-z", commit, "--", cfg.TrackedPath)
	if err != nil {
		return trackedGitSnapshot{}, wrapDiag("git_tracked_path", "inspect tracked path mode", err)
	}
	meta, _, ok := strings.Cut(strings.TrimSuffix(ls, "\x00"), "\t")
	if !ok {
		return trackedGitSnapshot{}, diag("git_tracked_path", "tracked path %q is absent from commit %s", cfg.TrackedPath, commit)
	}
	fields := strings.Fields(meta)
	if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || fields[2] != blob {
		return trackedGitSnapshot{}, diag("git_tracked_path", "tracked path %q must be one regular file (mode 100644 or 100755)", cfg.TrackedPath)
	}
	mode := os.FileMode(0o644)
	if fields[0] == "100755" {
		mode = 0o755
	}
	digest := sha256.Sum256(data)
	return trackedGitSnapshot{
		Repository: cfg.URL,
		Ref:        cfg.Ref,
		Path:       cfg.TrackedPath,
		Repo:       repo,
		Commit:     commit,
		Blob:       blob,
		BlobSHA256: hex.EncodeToString(digest[:]),
		Bytes:      data,
		Mode:       mode,
		Timestamp:  gitObjectTimestamp(repo, commit),
	}, nil
}

func validateTrackedGitLock(entry LockSource, cfg GitSourceConfig, version string) error {
	if entry.kind() != "git" || !entry.isTrackedGit() {
		return diag("lock_kind_mismatch", "version %q is not locked as a tracked-path git snapshot", version)
	}
	if entry.Repository != cfg.URL || entry.Ref != cfg.Ref || entry.Path != cfg.TrackedPath {
		return diag("lock_mismatch", "tracked git lock for version %q binds %q @ %q path %q, but the recipe requests %q @ %q path %q", version, entry.Repository, entry.Ref, entry.Path, cfg.URL, cfg.Ref, cfg.TrackedPath)
	}
	if !trackedGitObjectRE.MatchString(entry.Commit) || !trackedGitObjectRE.MatchString(entry.Blob) || len(entry.BlobSHA256) != sha256.Size*2 {
		return diag("lock_mismatch", "tracked git lock for version %q is missing its commit, blob, or blob_sha256 assertion", version)
	}
	if _, err := hex.DecodeString(entry.BlobSHA256); err != nil {
		return diag("lock_mismatch", "tracked git lock for version %q has an invalid blob_sha256 assertion", version)
	}
	if trackedSnapshotVersionRE.FindStringSubmatch(version) == nil {
		return diag("lock_version", "tracked git lock version %q is not YYYY.MM.DD or YYYY.MM.DD.N", version)
	}
	return nil
}

func (e LockSource) isTrackedGit() bool {
	return e.Repository != "" || e.Path != "" || e.Blob != "" || e.BlobSHA256 != ""
}

func snapshotMatchesLock(snapshot trackedGitSnapshot, entry LockSource) error {
	if snapshot.Commit != entry.Commit || snapshot.Blob != entry.Blob || snapshot.BlobSHA256 != entry.BlobSHA256 {
		return diag("lock_mismatch", "tracked git source for version %q does not match its locked commit/blob/digest", entry.Version)
	}
	return nil
}

func applyTrackedGitLock(ctx *Context, recipe RecipeConfig, snapshot trackedGitSnapshot) error {
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return err
	}
	if existing := lock.Find(snapshot.Version); existing != nil {
		if err := validateTrackedGitLock(*existing, recipe.Source.Git, snapshot.Version); err != nil {
			return err
		}
		return snapshotMatchesLock(snapshot, *existing)
	}
	lock.Put(LockSource{
		Version:    snapshot.Version,
		Repository: snapshot.Repository,
		Ref:        snapshot.Ref,
		Path:       snapshot.Path,
		Commit:     snapshot.Commit,
		Blob:       snapshot.Blob,
		BlobSHA256: snapshot.BlobSHA256,
		LockedAt:   lockTimestamp(ctx),
	})
	if err := SaveLockFile(recipe.Root, lock); err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "lock", Version: snapshot.Version, Path: lockFilePath(recipe.Root), Message: "pinned tracked path " + snapshot.Path + " at commit " + snapshot.Commit + " blob " + snapshot.Blob + " sha256:" + snapshot.BlobSHA256})
	return nil
}

func resolveTrackedGitSource(ctx *Context, recipe RecipeConfig, outBase string, cfg GitSourceConfig, version Version) (SourceState, error) {
	if version.Raw == "" {
		return SourceState{}, diag("missing_version", "tracked-path git sources require --latest, --all-versions, or an exact --version")
	}
	lock, history, err := trackedGitHistory(recipe, cfg)
	if err != nil {
		return SourceState{}, err
	}
	entry := lock.Find(version.Raw)
	var snapshot trackedGitSnapshot
	if entry != nil {
		if err := validateTrackedGitLock(*entry, cfg, version.Raw); err != nil {
			return SourceState{}, err
		}
		repo := trackedGitRepo(recipe, cfg)
		if ctx.Inv.DryRun {
			scope := "git-tracked-" + shortHash(cfg.URL, entry.Commit, cfg.TrackedPath, entry.BlobSHA256)
			st := drySource("git", outBase, scope, "git:"+cfg.URL+"@"+entry.Commit+":"+cfg.TrackedPath+"#sha256:"+entry.BlobSHA256)
			st.GitRepo, st.Commit, st.TrackedPath = repo, entry.Commit, cfg.TrackedPath
			return st, nil
		}
		if ctx.Inv.RefreshSource {
			if err := os.RemoveAll(repo); err != nil {
				return SourceState{}, wrapDiag("clean_source", repo, err)
			}
		}
		if err := ensureTrackedGitRepository(repo, cfg.URL); err != nil {
			return SourceState{}, err
		}
		if err := ensureTrackedGitCommit(repo, cfg, entry.Commit); err != nil {
			return SourceState{}, err
		}
		snapshot, err = inspectTrackedGitObject(repo, cfg, entry.Commit)
		if err != nil {
			return SourceState{}, err
		}
		snapshot.Version = version.Raw
		if err := snapshotMatchesLock(snapshot, *entry); err != nil {
			return SourceState{}, err
		}
	} else {
		key := trackedGitKey(recipe, cfg) + "\x00" + version.Raw
		var ok bool
		snapshot, ok = ctx.TrackedGit[key]
		if !ok {
			snapshot, err = discoverTrackedGitSnapshot(ctx, recipe, cfg, lock, history)
			if err != nil {
				return SourceState{}, err
			}
		}
		if snapshot.Version == "" || snapshot.Version != version.Raw {
			return SourceState{}, diag("tracked_git_version", "version %q is not a locked tracked-path snapshot and does not identify the current changed blob; run `pekit lock --latest` to discover it", version.Raw)
		}
		if ctx.Inv.DryRun {
			scope := "git-tracked-" + shortHash(cfg.URL, snapshot.Commit, cfg.TrackedPath, snapshot.BlobSHA256)
			st := drySource("git", outBase, scope, "git:"+cfg.URL+"@"+snapshot.Commit+":"+cfg.TrackedPath+"#sha256:"+snapshot.BlobSHA256)
			st.GitRepo, st.Commit, st.TrackedPath = snapshot.Repo, snapshot.Commit, cfg.TrackedPath
			return st, nil
		}
		if err := applyTrackedGitLock(ctx, recipe, snapshot); err != nil {
			return SourceState{}, err
		}
	}

	scope := "git-tracked-" + shortHash(cfg.URL, snapshot.Commit, cfg.TrackedPath, snapshot.BlobSHA256)
	workBase := filepath.Join(outBase, scope)
	sourceRoot := filepath.Join(workBase, "source")
	if err := os.RemoveAll(sourceRoot); err != nil {
		return SourceState{}, wrapDiag("clean_source", sourceRoot, err)
	}
	dest := filepath.Join(sourceRoot, filepath.FromSlash(cfg.TrackedPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return SourceState{}, wrapDiag("mkdir", filepath.Dir(dest), err)
	}
	if err := os.WriteFile(dest, snapshot.Bytes, snapshot.Mode); err != nil {
		return SourceState{}, wrapDiag("write_file", dest, err)
	}
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
	provenance := "git:" + cfg.URL + "@" + snapshot.Commit + ":" + cfg.TrackedPath + "#sha256:" + snapshot.BlobSHA256
	if err := writeSourceManifest(filepath.Join(workBase, "source.pekit.json"), SourceManifest{
		Kind:          "git",
		Rendered:      cfg.URL + "@" + cfg.Ref + ":" + cfg.TrackedPath,
		Immutable:     snapshot.Commit + ":" + snapshot.Blob,
		Checksum:      snapshot.BlobSHA256,
		Root:          cfg.TrackedPath,
		SourceRoot:    sourceRoot,
		ProvenanceRef: provenance,
		Timestamp:     snapshot.Timestamp,
		Patches:       patchesHash,
	}); err != nil {
		return SourceState{}, err
	}
	return SourceState{
		Kind:          "git",
		Scope:         scope,
		OutBase:       outBase,
		WorkBase:      workBase,
		SourceRoot:    sourceRoot,
		LiteralRoot:   sourceRoot,
		ProvenanceRef: provenance,
		Timestamp:     snapshot.Timestamp,
		GitRepo:       snapshot.Repo,
		Commit:        snapshot.Commit,
		TrackedPath:   cfg.TrackedPath,
	}, nil
}
