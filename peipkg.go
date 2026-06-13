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
func peipkgEngine(job PackageJob) error {
	pf := job.Pkg
	if pf.Version == "" {
		return fmt.Errorf("package %s: format peipkg requires [package] version", job.Name)
	}
	if pf.Architecture == "" {
		return fmt.Errorf("package %s: format peipkg requires [package] architecture", job.Name)
	}

	files := make(map[string]string, len(job.Files))
	for _, sf := range job.Files {
		files[sf.Dest] = sf.Source
	}
	if err := pack.ValidateFiles(pf.Architecture, files); err != nil {
		return fmt.Errorf("package %s: payload layout: %w", job.Name, err)
	}

	build, err := localProvenance(job.Root)
	if err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}

	// Farm naming convention: name_version_arch.peipkg
	// (e.g. kernel_0.20.0-alpha1-1_x86_64.peipkg).
	outPath := filepath.Join(job.OutStage,
		fmt.Sprintf("%s_%s_%s.peipkg", job.Name, pf.Version, pf.Architecture))
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	defer f.Close()

	err = pack.Pack(pack.PackOptions{
		Manifest: packManifest(job.Name, pf, build),
		Files:    files,
		Out:      f,
	})
	if err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("package %s: %w", job.Name, err)
	}
	fmt.Printf("pekit: wrote %s\n", outPath)
	return nil
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

func gitOut(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
