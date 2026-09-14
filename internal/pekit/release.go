package pekit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/peios/peipkg/repopub"
)

type releaseArtifact struct{ Name, Version, Architecture, Path, SHA256 string }
type releaseReceipt struct {
	Member, Version, Source, Environment, RecipeRef string
	Gates                                           []string
	LintRules                                       []string
	Allowed                                         []lintFinding
	Inputs                                          map[string]string
	EnvironmentFiles                                map[string]string
	Artifacts                                       []releaseArtifact
}
type releaseBuild struct {
	Directory string
	Session   *releaseSession
	Receipt   releaseReceipt
	Inputs    *sourceInputs
}
type releaseSession struct {
	mu                                      sync.Mutex
	Directory, Workspace, Commit, BaseState string
	Config                                  ReleaseConfig
	Builds                                  []*releaseBuild
	Artifacts                               []releaseArtifact
	PackageKeys                             []ed25519.PublicKey
}

func releaseHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil)), e
}
func releaseJSON(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}
func releaseCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) != nil {
		_, err = io.Copy(out, in)
	}
	ce := out.Close()
	if err != nil {
		return err
	}
	return ce
}
func gitReleaseOutput(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	b, e := cmd.Output()
	return strings.TrimSpace(string(b)), e
}
func preflightRelease(ctx *Context, recipe RecipeConfig, ws *WorkspaceConfig) error {
	if ws == nil || ws.Release.Path == "" {
		return diag("release_policy_missing", "release requires workspace [release] policy")
	}
	if !ws.Isolation.Enabled {
		return diag("release_isolation_required", "release requires isolated build workers")
	}
	if ctx.Inv.NoGates || ctx.Inv.NoBuild != nil || ctx.Inv.NoVerify != nil || ctx.Inv.AllowUnsigned || ctx.Inv.AllowUnanchored || ctx.Inv.Local != nil || ctx.Inv.PreferLocal != nil || ctx.Inv.Repin {
		return diag("release_bypass", "release does not allow build, verification or provenance bypasses")
	}
	ctx.Inv.EnvName = ws.Release.Environments[0]
	if ctx.Inv.DryRun {
		return nil
	}
	return ctx.Release.init(ctx, ws)
}
func (s *releaseSession) init(ctx *Context, ws *WorkspaceConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Directory != "" {
		if s.Workspace != ws.Root {
			return fmt.Errorf("release cannot span different workspaces")
		}
		return nil
	}
	commit, err := gitReleaseOutput(ws.Root, "rev-parse", "HEAD")
	if err != nil {
		return diag("release_commit_required", "release needs a committed catalogue")
	}
	status, err := releaseCatalogueStatus(ws.Root)
	if err != nil {
		return err
	}
	if status != "" {
		return diag("release_dirty", "commit reviewed catalogue changes before selecting a production candidate")
	}
	// State capture precedes source selection and all build/test work.
	target := filepath.Join(ws.Root, ws.Release.Path)
	state := "absent"
	if _, err = os.Stat(target); err == nil {
		state, err = repopub.StateDigest(target)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return err
	}
	store := filepath.Join(ws.Root, ".pekit", "releases")
	if err = os.MkdirAll(store, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(store, "candidate-")
	if err != nil {
		return err
	}
	s.Directory, s.Workspace, s.Commit, s.BaseState, s.Config = dir, ws.Root, commit, state, ws.Release
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	tool, err := releaseHash(exe)
	if err != nil {
		return err
	}
	if err = releaseJSON(filepath.Join(dir, "selection.json"), map[string]any{"schema": 1, "catalogue_commit": commit, "base_repository": state, "policy": ws.Release, "builder": pekitBuilder(), "builder_sha256": tool}); err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "release_candidate", Path: dir, Message: "recording fixed release candidate"})
	return nil
}
func releaseRecipe(ctx *Context, recipe RecipeConfig, ws *WorkspaceConfig, source SourceState, version Version, member string) error {
	if source.Local || source.Unanchored {
		return diag("release_provenance", "release requires anchored upstream sources")
	}
	if ctx.Inv.DryRun {
		ctx.Renderer.Event(Event{Type: "release_plan", Member: member, Version: version.Raw, Message: "would qualify this source in " + strings.Join(ws.Release.Environments, ", ") + " and promote one checked batch"})
		return nil
	}
	s := ctx.Release
	id := sha256.Sum256([]byte(recipe.Root + "\x00" + source.ProvenanceRef + "\x00" + version.Raw))
	// Freeze the prepared upstream tree once as well as its locked identity.
	// Each environment receives its own disposable copy of these same bytes.
	secrets := map[string]bool{}
	rings, err := resolveKeyrings(ctx.Inv, recipe.Root, ws)
	if err != nil {
		return err
	}
	for _, value := range rings {
		if p, e := absPath(ctx.Inv.Cwd, value); e == nil {
			secrets[p] = true
		}
	}
	frozenSource := filepath.Join(source.OutBase, ".pekit-release-sources", filepath.Base(s.Directory), fmt.Sprintf("%x", id[:12]))
	if err = snapshotTree(source.SourceRoot, frozenSource, source.OutBase, sourceSnapshotExclusions(ws.Root, secrets)); err != nil {
		return err
	}
	source.SourceRoot = frozenSource
	var first *sourceInputs
	for i, env := range ws.Release.Environments {
		if first != nil {
			if err := first.unchanged(); err != nil {
				return err
			}
		}
		b := &releaseBuild{Session: s, Directory: filepath.Join(s.Directory, fmt.Sprintf("%x", id[:12]), env)}
		if err := os.MkdirAll(b.Directory, 0700); err != nil {
			return err
		}
		local := *ctx
		local.Inv = ctx.Inv
		local.Inv.Command = CommandPackage
		local.Inv.DelegateCommand = CommandPackage
		local.Inv.EnvName = env
		local.ReleaseBuild = b
		local.Jobs = map[string]*buildJob{}
		// A unique stage per environment prevents reference artifacts or previous
		// job success markers from masquerading as the native candidate.
		scoped := source
		scoped.WorkBase = filepath.Join(source.OutBase, ".pekit-release-builds", filepath.Base(s.Directory), fmt.Sprintf("%x", id[:12]), env)
		log, err := os.OpenFile(filepath.Join(b.Directory, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		recorder := &releaseRenderer{Next: ctx.Renderer, File: log}
		local.Renderer = recorder
		if err = preflightVerify(&local, recipe, ws, member); err == nil {
			err = packageOrPublish(&local, recipe, ws, scoped, version, false, member)
		}
		if err != nil {
			recorder.Error(err)
		}
		closeErr := log.Close()
		if err != nil {
			return err
		}
		if recorder.Err != nil {
			return recorder.Err
		}
		if closeErr != nil {
			return closeErr
		}
		if first == nil {
			first = b.Inputs
		}
		s.mu.Lock()
		s.Builds = append(s.Builds, b)
		if i == len(ws.Release.Environments)-1 {
			s.Artifacts = append(s.Artifacts, b.Receipt.Artifacts...)
		}
		s.mu.Unlock()
	}
	return nil
}
func (b *releaseBuild) qualify(ctx *Context, recipe RecipeConfig, ws *WorkspaceConfig, source SourceState, version Version, instances []PackageInstance, inputs *sourceInputs, key ed25519.PrivateKey, member string) error {
	if key == nil {
		return diag("unsigned_release", "release requires signed packages")
	}
	if inputs == nil {
		return diag("release_inputs_missing", "release requires captured build inputs")
	}
	if err := inputs.unchanged(); err != nil {
		return err
	}
	b.Inputs = inputs
	cfg, err := loadLintConfig(recipe.Root, ws, source.SourceRoot)
	if err != nil {
		return err
	}
	if len(cfg.Files) == 0 || len(cfg.EnabledRules()) == 0 || len(cfg.enabledPayloadRules()) == 0 {
		return diag("release_lint_missing", "release requires active recipe and payload lint policy")
	}
	// Reference roots exercise portability against their own toolchains. Their
	// explicit exceptions never apply to the final environment's promoted bytes.
	publication := ctx.Inv.EnvName == ws.Release.Environments[len(ws.Release.Environments)-1]
	if !publication {
		for rule, reason := range ws.Release.ReferenceAllow {
			cfg.Allow[rule] = lintAllow{Reason: reason, Path: ws.Path}
		}
	}
	l := newLinter(ctx, cfg, member)
	if err = lintStatic(l, recipe, ws, source); err != nil {
		return err
	}
	var frozen []PackageInstance
	for i, inst := range instances {
		if inst.Format != "peipkg" {
			return diag("release_format", "release accepts peipkg archives only")
		}
		dst := filepath.Join(b.Directory, "artifacts", fmt.Sprintf("%04d-%s", i, filepath.Base(inst.Artifact)))
		if err = releaseCopy(inst.Artifact, dst); err != nil {
			return err
		}
		inst.Artifact = dst
		frozen = append(frozen, inst)
	}
	if err = lintReleaseArchives(l, frozen, key, b.Directory, source.WorkBase, version); err != nil {
		return err
	}
	if err = l.finish(); err != nil {
		return err
	}
	if err = inputs.unchanged(); err != nil {
		return err
	}
	b.Receipt = releaseReceipt{Member: member, Version: version.Raw, Source: source.ProvenanceRef, Environment: ctx.Inv.EnvName, RecipeRef: recipeRef(recipe.Root, recipeRefScope(recipe, ws, instances)), Inputs: inputs.Files, LintRules: cfg.EnabledRules(), Allowed: l.allowed, EnvironmentFiles: map[string]string{}}
	for _, g := range releaseGates(recipe) {
		b.Receipt.Gates = append(b.Receipt.Gates, g.Name)
	}
	job := ctx.Jobs[source.WorkBase]
	if job == nil {
		return diag("release_environment_missing", "release has no isolated job")
	}
	deps := filepath.Join(job.Directory, "dependencies")
	if len(job.Stages) > 0 && !dirExists(deps) {
		return diag("release_environment_missing", "root preparer did not retain dependency identities")
	}
	if dirExists(deps) {
		if err = snapshotTree(deps, filepath.Join(b.Directory, "build-environment"), "", job.Secrets); err != nil {
			return err
		}
		files, err := sourceTreeEntries(filepath.Join(b.Directory, "build-environment"), "")
		if err != nil {
			return err
		}
		for _, f := range files {
			id, err := inputIdentity(f.Source)
			if err != nil {
				return err
			}
			b.Receipt.EnvironmentFiles[f.Dest] = id
		}
	}
	// Retain the captured recipe/helper tree for sourceless packages too.
	if err = snapshotTree(inputs.Directory, filepath.Join(b.Directory, "inputs"), "", job.Secrets); err != nil {
		return err
	}
	for _, inst := range frozen {
		hash, err := releaseHash(inst.Artifact)
		if err != nil {
			return err
		}
		b.Receipt.Artifacts = append(b.Receipt.Artifacts, releaseArtifact{Name: inst.Name, Version: inst.Version, Architecture: inst.Architecture, Path: inst.Artifact, SHA256: hash})
	}
	b.Session.mu.Lock()
	b.Session.PackageKeys = append(b.Session.PackageKeys, key.Public().(ed25519.PublicKey))
	b.Session.mu.Unlock()
	return releaseJSON(filepath.Join(b.Directory, "qualification.json"), b.Receipt)
}

// Logs go to durable coordinator storage; a write failure invalidates evidence.
type releaseRenderer struct {
	mu   sync.Mutex
	Next Renderer
	File *os.File
	Err  error
}

func (r *releaseRenderer) Event(e Event) {
	r.mu.Lock()
	if r.Err == nil {
		r.Err = json.NewEncoder(r.File).Encode(e)
	}
	r.mu.Unlock()
	r.Next.Event(e)
}
func (r *releaseRenderer) Output(m, v, t, s, text string) {
	r.mu.Lock()
	if r.Err == nil {
		r.Err = json.NewEncoder(r.File).Encode(Event{Type: "output", Member: m, Version: v, Target: t, Stream: s, Text: text})
	}
	r.mu.Unlock()
	r.Next.Output(m, v, t, s, text)
}
func (r *releaseRenderer) Error(err error) { r.Event(Event{Type: "error", Message: err.Error()}) }
func (s *releaseSession) fail(err error) {
	if s.Directory != "" {
		_ = releaseJSON(filepath.Join(s.Directory, "FAILED.json"), map[string]string{"error": err.Error()})
	}
}

func (s *releaseSession) promote(ctx *Context) error {
	if len(s.Artifacts) == 0 {
		return diag("release_empty", "no qualified artifacts selected")
	}
	sort.Slice(s.Artifacts, func(i, j int) bool { return s.Artifacts[i].Path < s.Artifacts[j].Path })
	// Recheck every recipe's frozen input set after all workspace jobs finish.
	for _, b := range s.Builds {
		if err := b.Inputs.unchanged(); err != nil {
			return err
		}
	}
	commit, err := gitReleaseOutput(s.Workspace, "rev-parse", "HEAD")
	if err != nil || commit != s.Commit {
		return diag("release_stale", "catalogue commit changed during qualification")
	}
	status, e := releaseCatalogueStatus(s.Workspace)
	if e != nil {
		return e
	}
	if status != "" {
		return diag("release_stale", "catalogue files changed during qualification")
	}
	ws := &WorkspaceConfig{Root: s.Workspace}
	key, _, err := resolvePeipkgSigningKey(ctx, RecipeConfig{Root: s.Workspace}, ws, s.Config.SigningKey)
	if err != nil {
		return err
	}
	target := filepath.Join(s.Workspace, s.Config.Path)
	candidate := filepath.Join(s.Directory, "repository")
	if s.BaseState == "absent" {
		if err = repopub.Init(candidate, repopub.InitOptions{Name: s.Config.Name, Key: key, TrustedKeys: s.PackageKeys, GeneratedAt: ctx.App.Now().UTC()}); err != nil {
			return err
		}
	} else {
		if err = cloneReleaseRepository(target, candidate, s.BaseState); err != nil {
			return err
		}
	}
	base, err := repopub.StateDigest(candidate)
	if err != nil {
		return err
	}
	hashes := map[string]string{}
	var paths []string
	for _, a := range s.Artifacts {
		if _, ok := hashes[a.Path]; ok {
			return fmt.Errorf("release artifact selected twice")
		}
		hashes[a.Path] = a.SHA256
		paths = append(paths, a.Path)
	}
	q := &repopub.Qualification{BaseState: base, Artifacts: hashes}
	if _, err = repopub.Publish(candidate, repopub.PublishOptions{Key: key, Paths: paths, GeneratedAt: ctx.App.Now().UTC(), Qualification: q}); err != nil {
		return err
	}
	candidateState, err := repopub.StateDigest(candidate)
	if err != nil {
		return err
	}
	if err = releaseJSON(filepath.Join(s.Directory, "candidate.json"), map[string]any{"schema": 1, "catalogue_commit": s.Commit, "base_repository": s.BaseState, "candidate_repository": candidateState, "artifacts": s.Artifacts, "environments": s.Config.Environments}); err != nil {
		return err
	}
	// Bind evidence and input snapshots before running the trusted coordinator
	// integration checks. Checks receive fixed artifacts and candidate repository.
	sealed, err := releaseEvidenceHashes(s.Directory)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(s.Config.Checks) {
		command := s.Config.Checks[name]
		var cmd *exec.Cmd
		if command.Shell != "" {
			cmd = exec.Command("sh", "-euc", command.Shell)
		} else {
			cmd = exec.Command(command.Args[0], command.Args[1:]...)
		}
		cmd.Dir = s.Workspace
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.Directory, "TMPDIR=" + s.Directory, "PEKIT_RELEASE_DIR=" + s.Directory, "PEKIT_RELEASE_REPOSITORY=" + candidate}
		out, err := os.OpenFile(filepath.Join(s.Directory, "check-"+name+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		cmd.Stdout = out
		cmd.Stderr = out
		err = cmd.Run()
		ce := out.Close()
		if err != nil {
			return fmt.Errorf("release check %s failed: %w", name, err)
		}
		if ce != nil {
			return ce
		}
	}
	for p, h := range sealed {
		got, e := repopub.EvidenceIdentity(p)
		if e != nil || got != h {
			return diag("release_stale", "candidate evidence or artifacts changed during checks: %s", p)
		}
	}
	got, err := repopub.StateDigest(candidate)
	if err != nil || got != candidateState {
		return diag("release_stale", "candidate repository changed during checks")
	}
	for _, b := range s.Builds {
		if err = b.Inputs.unchanged(); err != nil {
			return err
		}
	}
	if status, e := releaseCatalogueStatus(s.Workspace); e != nil {
		return e
	} else if status != "" {
		return diag("release_stale", "catalogue files changed during release checks")
	}
	for _, name := range sortedKeys(s.Config.Checks) {
		p := filepath.Join(s.Directory, "check-"+name+".log")
		hash, e := repopub.EvidenceIdentity(p)
		if e != nil {
			return e
		}
		sealed[p] = hash
	}
	receipt := map[string]any{"schema": 1, "qualified": true, "catalogue_commit": s.Commit, "base_repository": s.BaseState, "candidate_repository": candidateState, "artifacts": s.Artifacts, "evidence": sealed, "checks": sortedKeys(s.Config.Checks)}
	if err = releaseJSON(filepath.Join(s.Directory, "release.json"), receipt); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(s.Directory, "release.json"))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(s.Directory, "release.json.sig"), ed25519.Sign(key, data), 0600); err != nil {
		return err
	}
	if s.BaseState == "absent" {
		// Init refuses an existing/nonempty repository. Never replace an operator's
		// repository merely because it was absent at the start of a long build.
		if _, err = os.Stat(target); !os.IsNotExist(err) {
			return diag("release_stale", "production repository appeared during qualification")
		}
		// Candidate already contains the checked signed indexes and packages. An
		// exclusive rename is implemented by publishing through an empty Init below.
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err = os.Mkdir(target, 0755); err != nil {
			return err
		}
		if err = repopub.Init(target, repopub.InitOptions{RequireQualification: true, Name: s.Config.Name, Key: key, TrustedKeys: s.PackageKeys, GeneratedAt: ctx.App.Now().UTC()}); err != nil {
			return err
		}
		q.BaseState, err = repopub.StateDigest(target)
		if err != nil {
			return err
		}
	} else {
		q.BaseState = s.BaseState
	}
	q.EvidencePath = filepath.Join(s.Directory, "release.json")
	result, err := repopub.Publish(target, repopub.PublishOptions{Key: key, Paths: paths, GeneratedAt: ctx.App.Now().UTC(), Qualification: q})
	if err != nil {
		return err
	}
	if err = releaseJSON(filepath.Join(s.Directory, "PROMOTED.json"), map[string]any{"repository": target, "index_version": result.IndexVersion, "receipt_sha256": fmt.Sprintf("%x", sha256.Sum256(data))}); err != nil {
		return fmt.Errorf("repository promoted at index %d but completion receipt failed: %w", result.IndexVersion, err)
	}
	ctx.Renderer.Event(Event{Type: "publish", Path: target, Message: fmt.Sprintf("promoted qualified release at index %d; evidence: %s", result.IndexVersion, s.Directory)})
	return nil
}

// Snapshot archives have independent inodes. Reflinks save space where the
// filesystem supports them; a check can never write through to live archives.
func cloneReleaseRepository(src, dst, expected string) error {
	before, err := repopub.StateDigest(src)
	if err != nil {
		return err
	}
	if before != expected {
		return diag("release_stale", "production repository changed during builds")
	}
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0700)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("repository snapshot rejects nonregular file %s", p)
		}
		if strings.HasSuffix(p, ".peipkg") {
			return releaseCopy(p, to)
		}
		return releaseCopy(p, to)
	})
	if err != nil {
		return err
	}
	after, err := repopub.StateDigest(src)
	if err != nil {
		return err
	}
	copied, err := repopub.StateDigest(dst)
	if err != nil {
		return err
	}
	if after != expected || copied != expected {
		return diag("release_stale", "repository changed while snapshotting")
	}
	// The disposable preview is not a production target. Downgrade only its
	// copied publisher config so it can be assembled before evidence is signed.
	configPath := filepath.Join(dst, ".peipkg-repo.json")
	raw, e := os.ReadFile(configPath)
	if e != nil {
		return e
	}
	var config map[string]any
	if e = json.Unmarshal(raw, &config); e != nil {
		return e
	}
	config["schema_version"] = 1
	delete(config, "require_qualification")
	if e = releaseJSON(configPath, config); e != nil {
		return e
	}
	return nil
}
func releaseEvidenceHashes(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == dir {
			return nil
		}
		if p == filepath.Join(dir, "repository") {
			return filepath.SkipDir
		}
		id, e := repopub.EvidenceIdentity(p)
		if e != nil {
			return e
		}
		out[p] = id
		return nil
	})
	return out, err
}

func releaseCatalogueStatus(root string) (string, error) {
	return gitReleaseOutput(root, "status", "--porcelain", "--untracked-files=normal", "--", ".", ":(exclude)**/pekit.lock", ":(exclude)pekit.lock")
}
