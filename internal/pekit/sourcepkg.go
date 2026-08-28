package pekit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/peios/peipkg/pack"
)

// planSourcePackage decides whether a packaging run emits a
// corresponding-source package and synthesizes its instance. One source
// package covers every peipkg-format package the recipe produces: the
// members are builds of the same source, so their corresponding source
// is one artifact. Nil (with no error) means no source package: emission
// opted out, no reproducible external source, or no peipkg members.
func planSourcePackage(recipe RecipeConfig, source SourceState, instances []PackageInstance) (*PackageInstance, error) {
	if !recipe.SourcePackage.IsEnabled() {
		return nil, nil
	}
	if source.Local || (source.Kind != "git" && source.Kind != "url") {
		return nil, nil
	}
	// A git source without a resolved commit is a bare branch ref — a
	// deliberately moving target (see the lock rules) with no stable
	// corresponding source to package.
	if source.Kind == "git" && source.Commit == "" {
		return nil, nil
	}
	var members []PackageInstance
	for _, inst := range instances {
		if inst.Format == "peipkg" {
			members = append(members, inst)
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	versionText := members[0].Version
	homepage := members[0].Config.Package.Homepage
	licenses := map[string]bool{}
	licenseClass := ""
	for _, m := range members {
		if m.Version != versionText {
			return nil, diag("source_package_version_conflict",
				"source package needs one version but members disagree (%s vs %s); align member versions or set source_package.enabled = false",
				versionText, m.Version)
		}
		if m.Config.Package.Homepage != homepage {
			homepage = ""
		}
		if l := m.Config.Package.License; l != "" {
			licenses[l] = true
		}
		licenseClass = worseLicenseClass(licenseClass, m.Config.Package.LicenseClass)
	}
	name := recipe.SourcePackage.Name
	if name == "" {
		name = filepath.Base(recipe.Root) + "-source"
	}
	// The source package contains the members' licensed material, so its
	// license is the conjunction of theirs.
	license := strings.Join(sortedKeys(licenses), " AND ")
	base := strings.TrimSuffix(name, "-source")
	cfg := PackageConfig{
		Format: "peipkg",
		Package: PackageMeta{
			Name:         name,
			Version:      versionText,
			Architecture: "noarch",
			Description:  "Corresponding source for " + base + " " + versionText,
			License:      license,
			LicenseClass: licenseClass,
			Homepage:     homepage,
		},
		Publish: members[0].Config.Publish,
	}
	stageID := "source-" + shortHash(name, versionText, "noarch", "peipkg")
	stage := filepath.Join(source.WorkBase, "package", stageID)
	return &PackageInstance{
		DefinitionSelector: name,
		Config:             cfg,
		Name:               name,
		Version:            versionText,
		Architecture:       "noarch",
		Format:             "peipkg",
		Stage:              stage,
		Artifact:           filepath.Join(stage, fmt.Sprintf("%s_%s_noarch.peipkg", name, versionText)),
		SourcePkg:          true,
	}, nil
}

// sourcePayloadRoot is the archive-path prefix all of a source package's
// content lands under (PSD-009 §3.4.1 reserves usr/src/dist/ for it).
func sourcePayloadRoot(inst PackageInstance) string {
	base := strings.TrimSuffix(inst.Name, "-source")
	return "usr/src/dist/" + base + "-" + inst.Version
}

// writeSourcePackage stages and writes one corresponding-source package:
// upstream/ carries the pristine source input (the exact bytes the lock
// hash covers, or a git-archive export of the locked commit), recipe/
// carries the recipe directory's build-controlling files, and patches/
// carries the recipe's patch series when one exists.
func writeSourcePackage(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, inst PackageInstance, member string, run packRun) error {
	if err := os.RemoveAll(inst.Stage); err != nil {
		return wrapDiag("clean_stage", inst.Stage, err)
	}
	if err := os.MkdirAll(inst.Stage, 0o755); err != nil {
		return wrapDiag("mkdir", inst.Stage, err)
	}
	root := sourcePayloadRoot(inst)
	base := strings.TrimSuffix(inst.Name, "-source")
	var entries []payloadEntry
	switch source.Kind {
	case "url":
		if !fileExists(source.Artifact) {
			return diag("missing_source_artifact", "cached source artifact %s does not exist", source.Artifact)
		}
		entries = append(entries, payloadEntry{Source: source.Artifact, Dest: root + "/upstream/" + filepath.Base(source.Artifact)})
	case "git":
		// git archive re-exports the exact locked commit from the mirror
		// clone — deterministic for a commit, independent of the mutable
		// checkout, and the commit id rides along in a pax comment.
		archive := filepath.Join(inst.Stage, base+"-"+inst.Version+".tar")
		if err := runSimple(source.GitRepo, "git", "archive", "--format=tar",
			"--prefix="+base+"-"+inst.Version+"/", "-o", archive, source.Commit); err != nil {
			return wrapDiag("git_archive", "export source tree", err)
		}
		entries = append(entries, payloadEntry{Source: archive, Dest: root + "/upstream/" + filepath.Base(archive)})
	default:
		return diag("unsupported_source", "source package cannot be built from a %s source", source.Kind)
	}
	recipeEntries, err := sourceRecipeEntries(recipe, root)
	if err != nil {
		return err
	}
	entries = append(entries, recipeEntries...)
	if err := validatePayloadDestinations(entries); err != nil {
		return err
	}
	if err := writePeipkg(ctx, workspace, inst, source, entries, run); err != nil {
		return err
	}
	if run.SignKey != nil {
		ctx.Renderer.Event(Event{Type: "sign", Member: member, Package: instanceID(inst), Path: inst.Artifact, Message: "signed with key " + pack.SigningKeyFingerprint(run.SignKey)})
	}
	ctx.Renderer.Event(Event{Type: "artifact", Member: member, Package: instanceID(inst), Version: version.Raw, Path: inst.Artifact, Message: "wrote source package"})
	return nil
}

// sourceRecipeEntries collects the recipe-directory files that count as
// the "scripts used to control compilation" half of corresponding
// source: the recipe and package definitions, the source lock (which
// makes the shipped upstream artifact verifiable), pinned upstream
// signing keys, and the patch series. The allowlist deliberately never
// matches *.keyring.pekit.toml — developer key material stays out.
func sourceRecipeEntries(recipe RecipeConfig, root string) ([]payloadEntry, error) {
	items, err := os.ReadDir(recipe.Root)
	if err != nil {
		return nil, wrapDiag("read_dir", recipe.Root, err)
	}
	var entries []payloadEntry
	for _, item := range items {
		name := item.Name()
		if item.IsDir() {
			destPrefix := ""
			switch {
			case name == "packages.pekit" || name == "keys":
				destPrefix = root + "/recipe/" + name
			// The applied series ships under the fixed patches/ name even
			// when [source].patches picks a different directory.
			case name == "patches" || (recipe.Source.Patches != "" && name == recipe.Source.Patches):
				destPrefix = root + "/patches"
			default:
				continue
			}
			sub, err := sourceTreeEntries(filepath.Join(recipe.Root, name), destPrefix)
			if err != nil {
				return nil, err
			}
			entries = append(entries, sub...)
			continue
		}
		switch {
		case name == "pekit.toml", name == "pekit.lock",
			name == "package.pekit.toml", name == "env.pekit.toml",
			strings.HasSuffix(name, ".package.pekit.toml"),
			strings.HasSuffix(name, ".env.pekit.toml"):
			entries = append(entries, payloadEntry{Source: filepath.Join(recipe.Root, name), Dest: root + "/recipe/" + name})
		}
	}
	return entries, nil
}

func sourceTreeEntries(dir, destPrefix string) ([]payloadEntry, error) {
	var entries []payloadEntry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, payloadEntry{Source: path, Dest: destPrefix + "/" + filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, wrapDiag("walk", dir, err)
	}
	return entries, nil
}

// worseLicenseClass folds two members' licence classes into the class of
// a package carrying both: the source package contains every member's
// material, so it is as encumbered as its most encumbered member.
// proprietary > firmware > unknown > free — unknown outranks free because
// a member nobody has classified cannot vouch for the whole. An undeclared
// class ("") is unknown, and stays "" so the manifest omits the key.
func worseLicenseClass(a, b string) string {
	rank := map[string]int{"": 2, "unknown": 2, "free": 1, "firmware": 3, "proprietary": 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
