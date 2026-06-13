package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/peios/peipkg/pack"
)

// peipkgEngine packs the job into a signed-format (but locally
// unsigned) .peipkg via peipkg's public pack API — the format has
// exactly one implementation and this is not it.
func peipkgEngine(job PackageJob) (string, error) {
	pf := job.Pkg
	if pf.Version == "" {
		return "", fmt.Errorf("package %s: format peipkg requires [package] version", job.Name)
	}
	if pf.Architecture == "" {
		return "", fmt.Errorf("package %s: format peipkg requires [package] architecture", job.Name)
	}

	files := make(map[string]string, len(job.Files))
	for _, sf := range job.Files {
		files[sf.Dest] = sf.Source
	}
	if err := pack.ValidateFiles(pf.Architecture, files); err != nil {
		return "", fmt.Errorf("package %s: payload layout: %w", job.Name, err)
	}

	var build pack.BuildInfo
	if job.Local {
		// Dev build of a working copy — there is no pinned commit to anchor
		// to (the tree may be dirty or not even a git repo), so stamp a
		// visible localdev marker instead of reading git HEAD.
		build = localdevProvenance()
	} else if b, err := localProvenance(job.ProvenanceDir); err != nil {
		// No commit to anchor to (a sourceless recipe in an uncommitted
		// tree). Provenance is best-effort metadata, not a reason to refuse
		// to build: warn and stamp an unanchored marker. A committed recipe
		// (every farm build) gets real provenance.
		fmt.Fprintf(os.Stderr, "pekit: warning: package %s: %v; stamping unanchored provenance (no commit ref)\n", job.Name, err)
		build = unanchoredProvenance()
	} else {
		build = b
	}

	// Farm naming convention: name_version_arch.peipkg
	// (e.g. kernel_0.20.0-alpha1-1_x86_64.peipkg).
	outPath := filepath.Join(job.OutStage,
		fmt.Sprintf("%s_%s_%s.peipkg", job.Name, pf.Version, pf.Architecture))
	f, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("package %s: %w", job.Name, err)
	}
	defer f.Close()

	err = pack.Pack(pack.PackOptions{
		Manifest: packManifest(job.Name, pf, build),
		Files:    files,
		Out:      f,
	})
	if err != nil {
		return "", fmt.Errorf("package %s: %w", job.Name, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("package %s: %w", job.Name, err)
	}
	return outPath, nil
}

func packManifest(name string, pf *PackageFile, build pack.BuildInfo) pack.Manifest {
	overrides := make([]pack.SDOverride, 0, len(pf.SDOverrides))
	for _, o := range pf.SDOverrides {
		overrides = append(overrides, pack.SDOverride{Path: o.Path, SDDL: o.SDDL})
	}
	return pack.Manifest{
		Name:                 name,
		Version:              pf.Version,
		Architecture:         pf.Architecture,
		Description:          pf.Description,
		License:              pf.License,
		Homepage:             pf.Homepage,
		Dependencies:         packDeps(pf.Dependencies),
		OptionalDependencies: packDeps(pf.OptionalDependencies),
		Conflicts:            packDeps(pf.Conflicts),
		Provides:             packProvides(pf.Provides),
		Replaces:             packReplaces(pf.Replaces),
		SideEffects:          append([]string(nil), pf.SideEffects...),
		SDOverrides:          overrides,
		Build:                build,
	}
}

func packDeps(in []Dependency) []pack.Dependency {
	out := make([]pack.Dependency, 0, len(in))
	for _, d := range in {
		out = append(out, pack.Dependency{Name: d.Name, Constraint: d.Constraint, Arch: d.Arch})
	}
	return out
}

func packProvides(in []Provides) []pack.Provides {
	out := make([]pack.Provides, 0, len(in))
	for _, p := range in {
		out = append(out, pack.Provides{Name: p.Name, Version: p.Version})
	}
	return out
}

func packReplaces(in []Replaces) []pack.Replaces {
	out := make([]pack.Replaces, 0, len(in))
	for _, r := range in {
		out = append(out, pack.Replaces{Name: r.Name, Constraint: r.Constraint})
	}
	return out
}

// localProvenance derives §3.3.4 build provenance from the project's
// git state: HEAD's commit time (stable, so local packs stay
// reproducible per commit) and hash, with FarmID "local" marking the
// package as visibly non-farm. The farm pipeline supplies its own.
func localProvenance(root string) (pack.BuildInfo, error) {
	commitTime, err := gitOut(root, "log", "-1", "--format=%cI")
	if err != nil {
		return pack.BuildInfo{}, fmt.Errorf("build provenance needs a git commit: %w", err)
	}
	ts, err := time.Parse(time.RFC3339, commitTime)
	if err != nil {
		return pack.BuildInfo{}, fmt.Errorf("parsing HEAD commit time %q: %w", commitTime, err)
	}
	ref, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return pack.BuildInfo{}, fmt.Errorf("build provenance needs a git commit: %w", err)
	}
	return pack.BuildInfo{
		Timestamp: ts.UTC().Format(time.RFC3339),
		FarmID:    "local",
		SourceRef: ref,
	}, nil
}

// localdevProvenance is the build info for a --local dev build: a visible
// localdev FarmID and a wall-clock timestamp, with no commit ref — the
// working copy it was built from is not a reproducible point.
func localdevProvenance() pack.BuildInfo {
	return pack.BuildInfo{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		FarmID:    "localdev",
		SourceRef: "working-tree",
	}
}

// unanchoredProvenance is the build info for a package whose recipe dir has
// no git commit to anchor to. Like localProvenance it is a non-farm "local"
// build, but with no commit ref ("unknown") and a wall-clock timestamp — so,
// unlike a git-anchored build, it is not reproducible. Distinct from
// localdevProvenance ("localdev"/"working-tree"), which marks a --local
// working-copy build. The farm builds from committed recipes and never hits
// this path.
func unanchoredProvenance() pack.BuildInfo {
	return pack.BuildInfo{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		FarmID:    "local",
		SourceRef: "unknown",
	}
}

func gitOut(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
