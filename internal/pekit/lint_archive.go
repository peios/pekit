package pekit

import (
	"archive/tar"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/peios/peipkg/repopub"
)

// lintGate is the lint half of package and publish's gates. It runs the
// recipe's static rules, then the payload rules over the archives this run
// just wrote: the bytes that will ship, including symlinks and anything
// packing transformed, rather than a build stage that merely precedes them.
// --no-gates skips it together with the gated tests.
func lintGate(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version,
	instances []PackageInstance, signKey ed25519.PrivateKey, member string) error {

	cfg, err := loadLintConfig(recipe.Root, workspace, source.SourceRoot)
	if err != nil {
		return err
	}
	if len(cfg.Files) == 0 {
		ctx.Renderer.Event(Event{Type: "lint_skipped", Member: member, Version: version.Raw, Message: "no " + lintFileName + " applies; the lint gate has no rules to run"})
		return nil
	}
	if err := applyEnvLintAllow(ctx, recipe, workspace, &cfg); err != nil {
		return err
	}
	l := newLinter(ctx, cfg, member)
	if err := lintStatic(l, recipe, workspace, source); err != nil {
		return err
	}
	var trusted []ed25519.PublicKey
	if signKey != nil {
		trusted = append(trusted, signKey.Public().(ed25519.PublicKey))
	}
	scratch, err := os.MkdirTemp(source.WorkBase, ".lint-")
	if err != nil {
		return wrapDiag("mkdir", source.WorkBase, err)
	}
	defer os.RemoveAll(scratch)
	set := &lintPayloadSet{byName: map[string]*lintPackage{}, union: map[string]string{}, claims: map[string]bool{}, workBase: source.WorkBase, elfCache: map[string]*elfInfo{}}
	selected := map[string]bool{}
	for i, inst := range instances {
		selected[inst.DefinitionSelector] = true
		var pkg *lintPackage
		switch {
		case inst.Format == "peipkg":
			pkg, err = archiveLintPackage(inst, trusted, filepath.Join(scratch, fmt.Sprint(i)))
		case inst.Format == "tar" && !inst.SourcePkg:
			pkg, err = stagedLintPackage(inst, source, recipe, workspace, version)
		}
		if err != nil {
			return err
		}
		// Source packages are verified above, but the binary payload rules
		// (ELF, placement, executable paths) do not apply to them.
		if pkg == nil || inst.SourcePkg {
			continue
		}
		set.Packages = append(set.Packages, *pkg)
	}
	context := lintContextPackages(recipe, workspace, source, version, selected)
	for i := range set.Packages {
		set.add(&set.Packages[i])
	}
	for i := range context {
		set.add(&context[i])
	}
	for _, pkg := range set.Packages {
		lintPackagePayload(l, set, pkg, version)
	}
	return l.finish()
}

// add registers a package for cross-package lookups: the union of shipped
// paths, the name index and the claim slots.
func (s *lintPayloadSet) add(pkg *lintPackage) {
	s.byName[pkg.Inst.Name] = pkg
	for dest := range pkg.Files {
		s.union[dest] = pkg.Inst.Name
	}
	for _, side := range []map[string]map[string]ClaimSlot{pkg.Inst.Config.Package.Claims.Provides, pkg.Inst.Config.Package.Claims.Dependencies} {
		for _, slots := range side {
			for _, slot := range slots {
				if slot.Path == "" {
					continue
				}
				if p, err := claimPayloadPath(slot.Path); err == nil {
					s.claims[p] = true
				}
			}
		}
	}
}

// lintContextPackages resolves, from their build stages, the packages this
// run did not select. They are not linted, but a selected package's rules
// may look into them — a -devel symlink into the runtime package, a binary's
// debug file in -debuginfo — so a partial package run is judged against the
// same family a full one is. A package whose stage is absent is left out.
func lintContextPackages(recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, selected map[string]bool) []lintPackage {
	defs, err := loadEffectivePackages(recipe, workspace, source)
	if err != nil {
		return nil
	}
	var out []lintPackage
	for _, def := range defs {
		if selected[def.Selector] {
			continue
		}
		instances, err := expandPackageInstances(def, source, recipe, workspace, version)
		if err != nil {
			continue
		}
		for _, inst := range instances {
			if pkg, err := stagedLintPackage(inst, source, recipe, workspace, version); err == nil {
				out = append(out, *pkg)
			}
		}
	}
	return out
}

// stagedLintPackage is a package's payload as its file mappings resolve it
// from the build stages.
func stagedLintPackage(inst PackageInstance, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig, version Version) (*lintPackage, error) {
	entries, err := resolvePayloadEntries(inst, source, recipe, workspace, version)
	if err != nil {
		return nil, err
	}
	pkg := &lintPackage{Inst: inst, Files: map[string]payloadEntry{}}
	for _, entry := range entries {
		dest, err := cleanRelPath(entry.Dest)
		if err != nil {
			return nil, err
		}
		entry.Dest = dest
		pkg.Files[dest] = entry
	}
	pkg.Dests = sortedKeys(pkg.Files)
	return pkg, nil
}

// archiveLintPackage verifies one peipkg archive and materialises its payload
// for the payload rules. Each object lands at an unrelated flat path under dir,
// so no archive symlink can redirect extraction, and lint still sees each
// symlink's original target. The manifest's dependencies — declared and
// derived — replace the recipe's, since they are what the archive promises.
func archiveLintPackage(inst PackageInstance, trusted []ed25519.PublicKey, dir string) (*lintPackage, error) {
	info, err := repopub.InspectPackage(inst.Artifact, trusted)
	if err != nil {
		return nil, wrapDiag("lint_archive", inst.Artifact, err)
	}
	if len(trusted) > 0 && !info.Signed {
		return nil, diag("lint_archive", "%s is unsigned although a signing key is configured", inst.Artifact)
	}
	var metadata struct {
		Name, Version, Architecture string
		Dependencies                []struct{ Name, Constraint string }
	}
	if err = json.Unmarshal(info.ManifestJSON, &metadata); err != nil {
		return nil, wrapDiag("lint_archive", inst.Artifact, err)
	}
	if metadata.Name != inst.Name || metadata.Version != inst.Version || metadata.Architecture != inst.Architecture {
		return nil, diag("lint_archive", "%s does not carry the identity it was packed as", inst.Artifact)
	}
	if inst.SourcePkg {
		return &lintPackage{Inst: inst}, nil
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, wrapDiag("mkdir", dir, err)
	}
	inst.Config.Package.Dependencies = map[string]string{}
	for _, d := range metadata.Dependencies {
		inst.Config.Package.Dependencies[d.Name] = d.Constraint
	}
	pkg := &lintPackage{Inst: inst, Files: map[string]payloadEntry{}}
	for j, p := range info.Payload {
		flat := filepath.Join(dir, fmt.Sprint(j))
		if p.Directory {
			err = os.Mkdir(flat, 0o755)
		} else if p.Symlink {
			err = os.Symlink(p.LinkTarget, flat)
		}
		if err != nil {
			return nil, err
		}
		pkg.Files[p.Path] = payloadEntry{Source: flat, Dest: p.Path}
	}
	if err = extractArchiveFiles(inst.Artifact, pkg.Files); err != nil {
		return nil, err
	}
	for dest, entry := range pkg.Files {
		if _, err := os.Lstat(entry.Source); err != nil {
			return nil, fmt.Errorf("archive payload missing during lint: %s: %w", dest, err)
		}
	}
	pkg.Dests = sortedKeys(pkg.Files)
	return pkg, nil
}

// extractArchiveFiles writes the regular files of a peipkg archive to the flat
// paths files assigns them.
func extractArchiveFiles(artifact string, files map[string]payloadEntry) error {
	f, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer z.Close()
	tr := tar.NewReader(z)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		entry, ok := files[h.Name]
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(entry.Source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
}

// applyEnvLintAllow adds the selected environment's [lint.allow] exemptions,
// from the workspace's env file and the recipe's own. An exemption a lint file
// already states keeps that file's reason.
func applyEnvLintAllow(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, cfg *LintConfig) error {
	var roots []string
	if workspace != nil {
		roots = append(roots, workspace.Root)
	}
	if workspace == nil || workspace.Root != recipe.Root {
		roots = append(roots, recipe.Root)
	}
	for _, root := range roots {
		env, err := selectedEnvFile(ctx.Inv, root, true)
		if err != nil {
			return err
		}
		for rule, reason := range env.LintAllow {
			if _, ok := cfg.Allow[rule]; !ok {
				cfg.Allow[rule] = lintAllow{Reason: reason, Path: env.Path}
			}
		}
	}
	return nil
}
