package pekit

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/peios/peipkg/pack"
)

type EffectivePackage struct {
	Selector         string
	SelectedInstance string
	Config           PackageConfig
	Layers           []PackageLayer
}

type PackageInstance struct {
	DefinitionSelector string
	InstanceSelector   string
	Multipack          string
	Config             PackageConfig
	Name               string
	Version            string
	Architecture       string
	Format             string
	Stage              string
	Artifact           string
}

type payloadEntry struct {
	Source   string
	Dest     string
	Override bool
}

func packageOrPublish(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, publish bool, member string) error {
	packages, err := loadEffectivePackages(recipe, workspace, source)
	if err != nil {
		if ctx.Inv.DryRun && diagCode(err) == "missing_package" && recipe.Delegate.AllowsPackages() && source.SourceRoot != recipe.Root && !dirExists(source.SourceRoot) {
			ctx.Renderer.Event(Event{Type: "unresolved", Member: member, Version: version.Raw, Message: "delegated package discovery requires source materialization"})
			return nil
		}
		return err
	}
	selected, err := selectPackages(ctx.Inv, packages)
	if err != nil {
		return err
	}
	if ctx.Inv.Verbose {
		ctx.Renderer.Event(Event{Type: "package_selection", Member: member, Version: version.Raw, Message: "selected packages: " + strings.Join(effectivePackageSelectors(selected), ", ")})
	}
	if err := validateSelectedPackageConfigs(selected); err != nil {
		return err
	}
	if !ctx.Inv.DryRun {
		if err := preflightStaticPayloads(selected, source, recipe, workspace, version); err != nil {
			return err
		}
	}
	buildNames, err := inferPackageBuilds(selected)
	if err != nil {
		return err
	}
	buildTargets := make([]TargetConfig, 0, len(buildNames))
	for _, name := range buildNames {
		t, ok := recipe.Targets[CommandBuild][name]
		if !ok {
			return diag("missing_target", "package needs missing build target %q", name)
		}
		buildTargets = append(buildTargets, t)
	}
	buildOrder, err := topoBuildsForInvocation(ctx.Inv, source, recipe.Targets[CommandBuild], buildTargets)
	if err != nil {
		return err
	}
	for _, target := range buildOrder {
		if err := runTarget(ctx, recipe, workspace, source, version, target, member); err != nil {
			return err
		}
	}
	var instances []PackageInstance
	unresolved := false
	for _, def := range selected {
		expanded, err := expandPackageInstances(def, source, recipe, workspace, version)
		if err != nil {
			if ctx.Inv.DryRun {
				unresolved = true
				ctx.Renderer.Event(Event{Type: "unresolved", Member: member, Package: def.Selector, Version: version.Raw, Message: err.Error()})
				continue
			}
			return err
		}
		instances = append(instances, expanded...)
	}
	if len(instances) == 0 {
		if ctx.Inv.DryRun && unresolved {
			return nil
		}
		return diag("no_artifacts", "no package artifacts were planned")
	}
	if err := validateArtifactDestinations(instances); err != nil {
		return err
	}
	if err := validateEmittedPackageNames(instances); err != nil {
		return err
	}
	var publishOps []plannedPublish
	if publish {
		if source.Unanchored && !ctx.Inv.AllowUnanchored {
			return diag("unanchored_provenance", "publish from unanchored source provenance requires --allow-unanchored")
		}
		publishOps, err = planPublishOps(workspace, recipe, instances)
		if err != nil {
			return err
		}
		if err := reservePublishDestinations(ctx, publishOps, member); err != nil {
			return err
		}
	}
	if source.Unanchored && !publish {
		ctx.Renderer.Event(Event{Type: "warning", Member: member, Message: "package uses unanchored source provenance"})
	}
	for _, inst := range instances {
		if ctx.Inv.DryRun {
			ctx.Renderer.Event(Event{Type: "package_plan", Member: member, Package: instanceID(inst), Version: version.Raw, Path: inst.Artifact, Message: "would write package"})
			continue
		}
		if err := writePackage(ctx, recipe, workspace, source, version, inst, member); err != nil {
			return err
		}
	}
	for _, op := range publishOps {
		if err := publishLocalDir(ctx, op, member); err != nil {
			return err
		}
	}
	return nil
}

func reservePublishDestinations(ctx *Context, ops []plannedPublish, member string) error {
	for _, op := range ops {
		owner := instanceID(op.Instance)
		if member != "" {
			owner = member + ":" + owner
		}
		if err := ctx.PublishRegistry.Reserve(op.Dest, owner); err != nil {
			return err
		}
	}
	return nil
}

func loadEffectivePackages(recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState) ([]EffectivePackage, error) {
	var workspaceBases, sourceBases, recipeBases []PackageLayer
	var sourceMembers, recipeMembers []PackageLayer
	if workspace != nil {
		layers, err := LoadPackageLayers(workspace.Root, "workspace")
		if err != nil {
			return nil, err
		}
		for _, layer := range layers {
			if layer.Base {
				workspaceBases = append(workspaceBases, layer)
			}
		}
	}
	if recipe.Delegate.AllowsPackages() && source.SourceRoot != "" && source.SourceRoot != recipe.Root {
		layers, err := LoadPackageLayers(source.SourceRoot, "source")
		if err != nil {
			return nil, err
		}
		for _, layer := range layers {
			if layer.Base {
				sourceBases = append(sourceBases, layer)
			} else {
				sourceMembers = append(sourceMembers, layer)
			}
		}
	}
	layers, err := LoadPackageLayers(recipe.Root, "recipe")
	if err != nil {
		return nil, err
	}
	for _, layer := range layers {
		if layer.Base {
			recipeBases = append(recipeBases, layer)
		} else {
			recipeMembers = append(recipeMembers, layer)
		}
	}
	memberNames := map[string]bool{}
	for _, layer := range sourceMembers {
		memberNames[layer.Selector] = true
	}
	for _, layer := range recipeMembers {
		memberNames[layer.Selector] = true
	}
	var out []EffectivePackage
	if len(memberNames) == 0 {
		if len(sourceBases) == 0 && len(recipeBases) == 0 {
			return nil, diag("missing_package", "no package definitions found")
		}
		layers := appendPackageLayers(nil, workspaceBases, sourceBases, recipeBases)
		cfg := mergePackageLayers(layers)
		out = append(out, EffectivePackage{Selector: "main", Config: cfg, Layers: layers})
		return out, nil
	}
	names := sortedKeys(memberNames)
	for _, name := range names {
		layers := appendPackageLayers(nil, workspaceBases, sourceBases, recipeBases)
		memberHasName := false
		for _, layer := range sourceMembers {
			if layer.Selector == name {
				layers = append(layers, layer)
				if layer.Config.Package.Name != "" {
					memberHasName = true
				}
			}
		}
		for _, layer := range recipeMembers {
			if layer.Selector == name {
				layers = append(layers, layer)
				if layer.Config.Package.Name != "" {
					memberHasName = true
				}
			}
		}
		cfg := mergePackageLayers(layers)
		if !memberHasName {
			cfg.Package.Name = ""
		}
		out = append(out, EffectivePackage{Selector: name, Config: cfg, Layers: layers})
	}
	return out, nil
}

func appendPackageLayers(out []PackageLayer, groups ...[]PackageLayer) []PackageLayer {
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

func mergePackageLayers(layers []PackageLayer) PackageConfig {
	out := PackageConfig{Files: map[string]PackageFileEntry{}, Symlinks: map[string]PackageSymlinkEntry{}}
	for _, layer := range layers {
		out = mergePackageConfig(out, layer.Config)
	}
	return out
}

func mergePackageConfig(base, over PackageConfig) PackageConfig {
	out := base
	if over.Format != "" {
		out.Format = over.Format
	}
	if over.ClearOut != nil {
		out.ClearOut = over.ClearOut
	}
	if len(over.Builds) > 0 {
		out.Builds = append([]string(nil), over.Builds...)
	}
	out.Package = mergePackageMeta(out.Package, over.Package)
	if len(over.Files) > 0 {
		out.Files = cloneFiles(over.Files)
	}
	if len(over.Symlinks) > 0 {
		out.Symlinks = cloneSymlinkMap(over.Symlinks)
	}
	if len(over.Excludes) > 0 {
		out.Excludes = append([]string(nil), over.Excludes...)
	}
	if len(over.Multipack.Enum) > 0 || over.Multipack.EnumFiles.Path != "" {
		out.Multipack = over.Multipack
	}
	if len(over.Publish.LocalDir) > 0 {
		out.Publish = over.Publish
	}
	return out
}

func mergePackageMeta(base, over PackageMeta) PackageMeta {
	out := base
	if over.Name != "" {
		out.Name = over.Name
	}
	if over.Version != "" {
		out.Version = over.Version
	}
	if over.Architecture != "" {
		out.Architecture = over.Architecture
	}
	if over.Description != "" {
		out.Description = over.Description
	}
	if over.License != "" {
		out.License = over.License
	}
	if over.Homepage != "" {
		out.Homepage = over.Homepage
	}
	if len(over.Dependencies) > 0 {
		out.Dependencies = cloneStringMap(over.Dependencies)
	}
	if len(over.OptionalDependencies) > 0 {
		out.OptionalDependencies = cloneStringMap(over.OptionalDependencies)
	}
	if len(over.Conflicts) > 0 {
		out.Conflicts = cloneStringMap(over.Conflicts)
	}
	if len(over.Provides) > 0 {
		out.Provides = cloneStringMap(over.Provides)
	}
	if len(over.Replaces) > 0 {
		out.Replaces = cloneStringMap(over.Replaces)
	}
	if len(over.SideEffects) > 0 {
		out.SideEffects = append([]string(nil), over.SideEffects...)
	}
	if len(over.SDOverrides) > 0 {
		out.SDOverrides = cloneStringMap(over.SDOverrides)
	}
	return out
}

func selectPackages(inv Invocation, packages []EffectivePackage) ([]EffectivePackage, error) {
	if len(packages) == 0 {
		return nil, diag("missing_package", "no effective packages found")
	}
	if inv.All {
		return packages, nil
	}
	selectors := inv.selectors()
	if len(selectors) == 0 {
		if len(packages) == 1 {
			return packages, nil
		}
		return nil, diag("ambiguous_package", "multiple packages exist; select one or use --all: %s", strings.Join(packageSelectors(packages), ", "))
	}
	byName := map[string]EffectivePackage{}
	for _, pkg := range packages {
		byName[pkg.Selector] = pkg
	}
	out := make([]EffectivePackage, 0, len(selectors))
	for _, selector := range selectors {
		def, instance, hasInstance := strings.Cut(selector, ":")
		if err := validateSelector("package", def); err != nil {
			return nil, err
		}
		pkg, ok := byName[def]
		if !ok {
			return nil, diag("missing_package", "unknown package %q; available packages: %s", def, strings.Join(packageSelectors(packages), ", "))
		}
		if hasInstance {
			if err := validateSelector("package instance", instance); err != nil {
				return nil, err
			}
			pkg.SelectedInstance = instance
		}
		out = append(out, pkg)
	}
	return out, nil
}

func packageSelectors(packages []EffectivePackage) []string {
	out := make([]string, 0, len(packages))
	for _, pkg := range packages {
		out = append(out, pkg.Selector)
	}
	sortStrings(out)
	return out
}

func inferPackageBuilds(packages []EffectivePackage) ([]string, error) {
	seen := map[string]bool{}
	for _, pkg := range packages {
		for _, name := range pkg.Config.Builds {
			if err := validateSelector("target", name); err != nil {
				return nil, err
			}
			seen[name] = true
		}
		for src := range pkg.Config.Files {
			ref := parsePackageRef(src, "")
			if ref.Kind == "build" {
				seen[ref.Target] = true
			}
		}
		if pkg.Config.Multipack.EnumFiles.Path != "" {
			ref := parsePackageRef(pkg.Config.Multipack.EnumFiles.Path, "")
			if ref.Kind == "build" {
				seen[ref.Target] = true
			}
		}
	}
	return sortedKeys(seen), nil
}

func expandPackageInstances(pkg EffectivePackage, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig, version Version) ([]PackageInstance, error) {
	cfg := pkg.Config
	if !cfg.Multipack.Enabled() {
		if pkg.SelectedInstance != "" {
			return nil, diag("invalid_selector", "package %s is not a multipack package", pkg.Selector)
		}
		inst, err := makePackageInstance(pkg.Selector, "", "", cfg, source, version)
		if err != nil {
			return nil, err
		}
		return []PackageInstance{inst}, nil
	}
	values, err := enumerateMultipack(cfg.Multipack, source, recipe, workspace, version)
	if err != nil {
		return nil, err
	}
	out := make([]PackageInstance, 0, len(values))
	for _, value := range values {
		if pkg.SelectedInstance != "" && value != pkg.SelectedInstance {
			continue
		}
		inst, err := makePackageInstance(pkg.Selector, value, value, cfg, source, version)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	if pkg.SelectedInstance != "" && len(out) == 0 {
		return nil, diag("missing_package", "multipack package %s has no instance %q", pkg.Selector, pkg.SelectedInstance)
	}
	return out, nil
}

func makePackageInstance(selector, instanceSelector, multipack string, cfg PackageConfig, source SourceState, version Version) (PackageInstance, error) {
	ctx := TemplateContext{Version: version, Multipack: multipack}
	format := cfg.Format
	if format == "" {
		format = "tar"
	}
	name := cfg.Package.Name
	if name == "" {
		name = selector
		if multipack != "" {
			name += "-" + multipack
		}
	} else {
		rendered, err := RenderTemplate(name, ctx)
		if err != nil {
			return PackageInstance{}, wrapDiag("template", "render package.name", err)
		}
		name = rendered
	}
	versionText := cfg.Package.Version
	if versionText != "" {
		rendered, err := RenderTemplate(versionText, ctx)
		if err != nil {
			return PackageInstance{}, wrapDiag("template", "render package.version", err)
		}
		versionText = rendered
	}
	arch := cfg.Package.Architecture
	stageID := selector
	if instanceSelector != "" {
		stageID += "-" + instanceSelector
	}
	stageID += "-" + shortHash(name, versionText, arch, format)
	stage := filepath.Join(source.WorkBase, "package", stageID)
	artifact := filepath.Join(stage, name+".tar")
	if format == "peipkg" {
		artifact = filepath.Join(stage, fmt.Sprintf("%s_%s_%s.peipkg", name, versionText, arch))
	}
	return PackageInstance{
		DefinitionSelector: selector,
		InstanceSelector:   instanceSelector,
		Multipack:          multipack,
		Config:             cfg,
		Name:               name,
		Version:            versionText,
		Architecture:       arch,
		Format:             format,
		Stage:              stage,
		Artifact:           artifact,
	}, nil
}

func enumerateMultipack(cfg MultipackConfig, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig, version Version) ([]string, error) {
	if len(cfg.Enum) > 0 {
		return append([]string(nil), cfg.Enum...), nil
	}
	refText, err := RenderTemplate(cfg.EnumFiles.Path, TemplateContext{Version: version})
	if err != nil {
		return nil, err
	}
	root, pattern, err := resolveRefRoot(refText, "", source, recipe, workspace)
	if err != nil {
		return nil, err
	}
	matches, err := doublestar.Glob(os.DirFS(root), filepath.ToSlash(pattern))
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(cfg.EnumFiles.Regex)
	if err != nil {
		return nil, err
	}
	valueIndex, err := multipackCaptureIndex(re)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, match := range matches {
		m := re.FindStringSubmatch(filepath.Base(match))
		if m == nil || len(m) <= valueIndex {
			continue
		}
		value := m[valueIndex]
		if value == "" {
			continue
		}
		if err := validateSelector("multipack value", value); err != nil {
			return nil, err
		}
		seen[value] = true
	}
	if len(seen) == 0 {
		return nil, diag("empty_multipack", "multipack enumeration found no instances for %s", cfg.EnumFiles.Path)
	}
	return sortedKeys(seen), nil
}

func (cfg MultipackConfig) Enabled() bool {
	return len(cfg.Enum) > 0 || cfg.EnumFiles.Path != ""
}

func multipackCaptureIndex(re *regexp.Regexp) (int, error) {
	names := re.SubexpNames()
	valueIndex := -1
	for idx, name := range names {
		if name == "value" {
			if valueIndex != -1 {
				return 0, diag("invalid_regex", "multipack.enum.files.regex has multiple value capture groups")
			}
			valueIndex = idx
		}
	}
	if valueIndex != -1 {
		return valueIndex, nil
	}
	if re.NumSubexp() != 1 {
		return 0, diag("invalid_regex", "multipack.enum.files.regex must have exactly one capture group or one named value capture group")
	}
	return 1, nil
}

func writePackage(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, inst PackageInstance, member string) error {
	clearOut := true
	if inst.Config.ClearOut != nil {
		clearOut = *inst.Config.ClearOut
	}
	if clearOut {
		if err := os.RemoveAll(inst.Stage); err != nil {
			return wrapDiag("clean_stage", inst.Stage, err)
		}
	}
	if err := os.MkdirAll(inst.Stage, 0o755); err != nil {
		return wrapDiag("mkdir", inst.Stage, err)
	}
	entries, err := resolvePayloadEntries(inst, source, recipe, workspace, version)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return diag("empty_package", "package %s has no payload entries", instanceID(inst))
	}
	if ctx.Inv.Verbose {
		for _, entry := range entries {
			msg := entry.Source + " -> " + entry.Dest
			if entry.Override {
				msg += " (override)"
			}
			ctx.Renderer.Event(Event{Type: "package_file", Member: member, Package: instanceID(inst), Path: entry.Dest, Message: msg})
		}
	}
	if err := validatePayloadDestinations(entries); err != nil {
		return err
	}
	if inst.Format == "tar" {
		if err := validateTarPackageConfig(inst); err != nil {
			return err
		}
		if err := writeTar(inst.Artifact, entries); err != nil {
			return err
		}
	} else if inst.Format == "peipkg" {
		if err := writePeipkg(ctx, inst, source, entries); err != nil {
			return err
		}
	} else {
		return diag("unsupported_format", "unsupported package format %q", inst.Format)
	}
	ctx.Renderer.Event(Event{Type: "artifact", Member: member, Package: instanceID(inst), Version: version.Raw, Path: inst.Artifact, Message: "wrote package"})
	return nil
}

func effectivePackageSelectors(packages []EffectivePackage) []string {
	out := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if pkg.SelectedInstance != "" {
			out = append(out, pkg.Selector+":"+pkg.SelectedInstance)
		} else {
			out = append(out, pkg.Selector)
		}
	}
	sortStrings(out)
	return out
}

func validateArtifactDestinations(instances []PackageInstance) error {
	seen := map[string]string{}
	for _, inst := range instances {
		if prev, ok := seen[inst.Artifact]; ok {
			return diag("artifact_collision", "packages %s and %s write the same artifact %s", prev, instanceID(inst), inst.Artifact)
		}
		seen[inst.Artifact] = instanceID(inst)
	}
	return nil
}

func validateEmittedPackageNames(instances []PackageInstance) error {
	seen := map[string]string{}
	for _, inst := range instances {
		key := inst.Name
		if inst.Version != "" {
			key += "@" + inst.Version
		}
		if inst.Architecture != "" {
			key += "/" + inst.Architecture
		}
		if prev, ok := seen[key]; ok {
			return diag("package_name_collision", "packages %s and %s emit the same package name %s", prev, instanceID(inst), inst.Name)
		}
		seen[key] = instanceID(inst)
	}
	return nil
}

func validateSelectedPackageConfigs(packages []EffectivePackage) error {
	for _, pkg := range packages {
		format := pkg.Config.Format
		if format == "" {
			format = "tar"
		}
		switch format {
		case "tar":
			if err := validateTarPackageConfig(PackageInstance{DefinitionSelector: pkg.Selector, Config: pkg.Config}); err != nil {
				return err
			}
		case "peipkg":
			if pkg.Config.Package.Version == "" {
				return diag("missing_package_field", "peipkg package %s requires [package].version", pkg.Selector)
			}
			if pkg.Config.Package.Architecture == "" {
				return diag("missing_package_field", "peipkg package %s requires [package].architecture", pkg.Selector)
			}
		default:
			return diag("unsupported_format", "unsupported package format %q", format)
		}
	}
	return nil
}

func preflightStaticPayloads(packages []EffectivePackage, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig, version Version) error {
	for _, pkg := range packages {
		instances, err := expandPackageInstances(pkg, source, recipe, workspace, version)
		if err != nil {
			if code := diagCode(err); code == "empty_multipack" || code == "missing_payload" {
				continue
			}
			return err
		}
		for _, inst := range instances {
			ctx := TemplateContext{Version: version, Multipack: inst.Multipack}
			for rawSrc, file := range inst.Config.Files {
				srcRef, err := RenderTemplate(rawSrc, ctx)
				if err != nil {
					return wrapDiag("template", "render file source", err)
				}
				ref := parsePackageRef(srcRef, "")
				if ref.Kind == "build" {
					continue
				}
				dest, err := RenderTemplate(file.Path, ctx)
				if err != nil {
					return wrapDiag("template", "render file destination", err)
				}
				root, pattern, err := resolveRefRoot(srcRef, "", source, recipe, workspace)
				if err != nil {
					return err
				}
				excludes, err := renderExcludesForRoot(inst.Config.Excludes, ctx, root, source, recipe, workspace)
				if err != nil {
					return err
				}
				if _, err := expandPayloadMapping(root, pattern, dest, file.Override, excludes); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func resolvePayloadEntries(inst PackageInstance, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig, version Version) ([]payloadEntry, error) {
	var entries []payloadEntry
	ctx := TemplateContext{Version: version, Multipack: inst.Multipack}
	for rawSrc, file := range inst.Config.Files {
		srcRef, err := RenderTemplate(rawSrc, ctx)
		if err != nil {
			return nil, wrapDiag("template", "render file source", err)
		}
		dest, err := RenderTemplate(file.Path, ctx)
		if err != nil {
			return nil, wrapDiag("template", "render file destination", err)
		}
		root, pattern, err := resolveRefRoot(srcRef, "", source, recipe, workspace)
		if err != nil {
			return nil, err
		}
		excludes, err := renderExcludesForRoot(inst.Config.Excludes, ctx, root, source, recipe, workspace)
		if err != nil {
			return nil, err
		}
		mapped, err := expandPayloadMapping(root, pattern, dest, file.Override, excludes)
		if err != nil {
			return nil, err
		}
		entries = append(entries, mapped...)
	}
	if len(inst.Config.Symlinks) > 0 {
		linkRoot := filepath.Join(inst.Stage, "_pekit_symlinks")
		for rawDest, link := range inst.Config.Symlinks {
			dest, err := RenderTemplate(rawDest, ctx)
			if err != nil {
				return nil, err
			}
			target, err := RenderTemplate(link.Target, ctx)
			if err != nil {
				return nil, err
			}
			if target == "" || strings.ContainsRune(target, '\x00') {
				return nil, diag("invalid_symlink", "symlink %s has invalid target text", dest)
			}
			rel, err := cleanRelPath(dest)
			if err != nil {
				return nil, err
			}
			linkPath := filepath.Join(linkRoot, rel)
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				return nil, wrapDiag("mkdir", filepath.Dir(linkPath), err)
			}
			_ = os.Remove(linkPath)
			if err := os.Symlink(target, linkPath); err != nil {
				return nil, wrapDiag("symlink", linkPath, err)
			}
			entries = append(entries, payloadEntry{Source: linkPath, Dest: rel, Override: link.Override})
		}
	}
	return entries, nil
}

func validatePayloadDestinations(entries []payloadEntry) error {
	seen := map[string]string{}
	for _, entry := range entries {
		dest, err := cleanRelPath(entry.Dest)
		if err != nil {
			return wrapDiag("invalid_payload_path", entry.Dest, err)
		}
		entry.Dest = dest
		if prev, ok := seen[dest]; ok {
			return diag("payload_collision", "multiple package inputs map to %s: %s and %s", dest, prev, entry.Source)
		}
		info, err := os.Lstat(entry.Source)
		if err != nil {
			return wrapDiag("stat", entry.Source, err)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return diag("unsupported_payload_type", "package source %s is not a regular file or symlink", entry.Source)
		}
		seen[dest] = entry.Source
	}
	return nil
}

func validateTarPackageConfig(inst PackageInstance) error {
	meta := inst.Config.Package
	if meta.Version != "" || meta.Architecture != "" || meta.Description != "" || meta.License != "" || meta.Homepage != "" ||
		len(meta.Dependencies) > 0 || len(meta.OptionalDependencies) > 0 || len(meta.Conflicts) > 0 ||
		len(meta.Provides) > 0 || len(meta.Replaces) > 0 || len(meta.SideEffects) > 0 || len(meta.SDOverrides) > 0 {
		return diag("unsupported_manifest_field", "tar package %s cannot express manifest metadata", instanceID(inst))
	}
	return nil
}

func expandPayloadMapping(root, pattern, dest string, override bool, excludes []string) ([]payloadEntry, error) {
	destIsDir := strings.HasSuffix(dest, "/")
	dest, err := cleanRelPath(dest)
	if err != nil {
		return nil, err
	}
	magic := hasGlobMagic(pattern)
	var matches []string
	if magic {
		matches, err = doublestar.Glob(os.DirFS(root), filepath.ToSlash(pattern))
		if err != nil {
			return nil, err
		}
	} else if fileExists(filepath.Join(root, pattern)) || dirExists(filepath.Join(root, pattern)) {
		matches = []string{filepath.ToSlash(pattern)}
	}
	if len(matches) == 0 {
		return nil, diag("missing_payload", "file source %s has no matches", filepath.Join(root, pattern))
	}
	matches = filterCoveredDirectoryMatches(root, matches)
	if len(matches) > 1 && !destIsDir {
		return nil, diag("invalid_payload_path", "destination %s must end with / when a source expands to multiple entries", dest)
	}
	base := globBase(pattern)
	var out []payloadEntry
	for _, match := range matches {
		abs := filepath.Join(root, filepath.FromSlash(match))
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, wrapDiag("stat", abs, err)
		}
		if info.IsDir() {
			err := filepath.WalkDir(abs, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(abs, path)
				sourceRel, _ := filepath.Rel(root, path)
				sourceRel = filepath.ToSlash(sourceRel)
				if excluded(sourceRel, excludes) {
					return nil
				}
				archivePath := filepath.ToSlash(filepath.Join(dest, rel))
				out = append(out, payloadEntry{Source: path, Dest: archivePath, Override: override})
				return nil
			})
			if err != nil {
				return nil, wrapDiag("walk", abs, err)
			}
			continue
		}
		archivePath := dest
		if destIsDir && !magic && len(matches) == 1 {
			archivePath = filepath.ToSlash(filepath.Join(dest, filepath.Base(match)))
		} else if magic || len(matches) > 1 {
			rel := match
			if base != "" {
				if r, err := filepath.Rel(filepath.FromSlash(base), filepath.FromSlash(match)); err == nil {
					rel = r
				}
			}
			archivePath = filepath.ToSlash(filepath.Join(dest, rel))
		}
		if excluded(match, excludes) {
			continue
		}
		out = append(out, payloadEntry{Source: abs, Dest: archivePath, Override: override})
	}
	return out, nil
}

func filterCoveredDirectoryMatches(root string, matches []string) []string {
	sortStrings(matches)
	var out []string
	var dirs []string
	for _, match := range matches {
		slashed := filepath.ToSlash(match)
		covered := false
		for _, dir := range dirs {
			if slashed != dir && strings.HasPrefix(slashed, dir+"/") {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(slashed)))
		if err == nil && info.IsDir() {
			dirs = append(dirs, slashed)
		}
		out = append(out, slashed)
	}
	return out
}

func writeTar(path string, entries []payloadEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(path), err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return wrapDiag("create", path, err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	tw := tar.NewWriter(f)
	for _, entry := range entries {
		info, err := os.Lstat(entry.Source)
		if err != nil {
			return wrapDiag("stat", entry.Source, err)
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(entry.Source)
			if err != nil {
				return wrapDiag("readlink", entry.Source, err)
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return wrapDiag("tar", entry.Source, err)
		}
		hdr.Name = filepath.ToSlash(entry.Dest)
		hdr.ModTime = time.Unix(0, 0).UTC()
		hdr.AccessTime = time.Unix(0, 0).UTC()
		hdr.ChangeTime = time.Unix(0, 0).UTC()
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		if err := tw.WriteHeader(hdr); err != nil {
			return wrapDiag("tar", entry.Dest, err)
		}
		if info.Mode().IsRegular() {
			if err := copyToTar(tw, entry.Source); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return wrapDiag("tar", path, err)
	}
	if err := f.Close(); err != nil {
		return wrapDiag("tar", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return wrapDiag("rename", path, err)
	}
	return nil
}

func copyToTar(tw *tar.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return wrapDiag("open", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return wrapDiag("tar", path, err)
	}
	return nil
}

func writePeipkg(ctx *Context, inst PackageInstance, source SourceState, entries []payloadEntry) error {
	if inst.Version == "" {
		return diag("missing_package_field", "peipkg package %s requires [package].version", instanceID(inst))
	}
	if inst.Architecture == "" {
		return diag("missing_package_field", "peipkg package %s requires [package].architecture", instanceID(inst))
	}
	files := map[string]string{}
	validateFiles := map[string]string{}
	for _, entry := range entries {
		files[entry.Dest] = entry.Source
		if !entry.Override {
			validateFiles[entry.Dest] = entry.Source
		}
	}
	if len(validateFiles) > 0 {
		if err := pack.ValidateFiles(inst.Architecture, validateFiles); err != nil {
			return wrapDiag("payload_validation", instanceID(inst), err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(inst.Artifact), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(inst.Artifact), err)
	}
	f, err := os.CreateTemp(filepath.Dir(inst.Artifact), "."+filepath.Base(inst.Artifact)+".tmp-*")
	if err != nil {
		return wrapDiag("create", inst.Artifact, err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	meta := inst.Config.Package
	manifest := pack.Manifest{
		Name:                 inst.Name,
		Version:              inst.Version,
		Architecture:         inst.Architecture,
		Description:          meta.Description,
		License:              meta.License,
		Homepage:             meta.Homepage,
		Dependencies:         packDeps(meta.Dependencies),
		OptionalDependencies: packDeps(meta.OptionalDependencies),
		Conflicts:            packDeps(meta.Conflicts),
		Provides:             packProvides(meta.Provides),
		Replaces:             packReplaces(meta.Replaces),
		SideEffects:          append([]string(nil), meta.SideEffects...),
		SDOverrides:          packSDOverrides(meta.SDOverrides),
		Build: pack.BuildInfo{
			Timestamp: ctx.Start.UTC().Format(time.RFC3339),
			FarmID:    "local",
			SourceRef: source.ProvenanceRef,
		},
	}
	if err := pack.Pack(pack.PackOptions{Manifest: manifest, Files: files, Out: f}); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return wrapDiag("create", inst.Artifact, err)
	}
	if err := os.Rename(tmpPath, inst.Artifact); err != nil {
		return wrapDiag("rename", inst.Artifact, err)
	}
	return nil
}

type plannedPublish struct {
	Instance  PackageInstance
	Target    LocalDirPublish
	Dest      string
	Overwrite bool
}

func planPublishOps(workspace *WorkspaceConfig, recipe RecipeConfig, instances []PackageInstance) ([]plannedPublish, error) {
	var ops []plannedPublish
	seen := map[string]plannedPublish{}
	for _, inst := range instances {
		if len(inst.Config.Publish.LocalDir) == 0 {
			return nil, diag("missing_publish_target", "package %s has no publish target", inst.DefinitionSelector)
		}
		for _, target := range inst.Config.Publish.LocalDir {
			dst, overwrite, err := renderLocalDirDestination(workspace, recipe, inst, target)
			if err != nil {
				return nil, err
			}
			if prev, ok := seen[dst]; ok {
				if prev.Instance.Artifact == inst.Artifact {
					continue
				}
				return nil, diagAt("publish_collision", dst, "publish destination collision between %s and %s", instanceID(prev.Instance), instanceID(inst))
			}
			if !overwrite && fileExists(dst) {
				return nil, diagAt("publish_exists", dst, "publish destination exists")
			}
			op := plannedPublish{Instance: inst, Target: target, Dest: dst, Overwrite: overwrite}
			seen[dst] = op
			ops = append(ops, op)
		}
	}
	return ops, nil
}

func renderLocalDirDestination(workspace *WorkspaceConfig, recipe RecipeConfig, inst PackageInstance, target LocalDirPublish) (string, bool, error) {
	base := recipe.Root
	if workspace != nil {
		base = workspace.Root
	}
	rendered, err := RenderTemplate(target.Path, TemplateContext{Multipack: inst.Multipack, Version: mustParseVersion(inst.Version)})
	if err != nil {
		return "", false, err
	}
	rel, err := cleanRelPath(rendered)
	if err != nil {
		return "", false, err
	}
	dstDir := filepath.Join(base, rel)
	dst := filepath.Join(dstDir, filepath.Base(inst.Artifact))
	overwrite := true
	if target.Overwrite != nil {
		overwrite = *target.Overwrite
	}
	return dst, overwrite, nil
}

func publishLocalDir(ctx *Context, op plannedPublish, member string) error {
	if ctx.Inv.DryRun {
		ctx.Renderer.Event(Event{Type: "publish_plan", Member: member, Package: instanceID(op.Instance), Path: op.Dest, Message: "would publish package"})
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(op.Dest), 0o755); err != nil {
		return wrapDiag("mkdir", filepath.Dir(op.Dest), err)
	}
	if !op.Overwrite && fileExists(op.Dest) {
		return diagAt("publish_exists", op.Dest, "publish destination exists")
	}
	if err := copyFile(op.Instance.Artifact, op.Dest); err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "publish", Member: member, Package: instanceID(op.Instance), Path: op.Dest, Message: "published package"})
	return nil
}

type packageRef struct {
	Kind   string
	Target string
	Path   string
}

func parsePackageRef(ref, defaultKind string) packageRef {
	if strings.HasPrefix(ref, "@recipe:") {
		return packageRef{Kind: "recipe", Path: strings.TrimPrefix(ref, "@recipe:")}
	}
	if strings.HasPrefix(ref, "@source:") {
		return packageRef{Kind: "source", Path: strings.TrimPrefix(ref, "@source:")}
	}
	if strings.HasPrefix(ref, "@workspace:") {
		return packageRef{Kind: "workspace", Path: strings.TrimPrefix(ref, "@workspace:")}
	}
	if before, after, ok := strings.Cut(ref, ":"); ok {
		target := before
		if target == "" {
			target = "main"
		}
		return packageRef{Kind: "build", Target: target, Path: after}
	}
	if defaultKind == "" {
		defaultKind = "recipe"
	}
	return packageRef{Kind: defaultKind, Path: ref}
}

func resolveRefRoot(ref, defaultKind string, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig) (string, string, error) {
	parsed := parsePackageRef(ref, defaultKind)
	cleaned, err := cleanRelPath(parsed.Path)
	if err != nil {
		return "", "", wrapDiag("invalid_ref", ref, err)
	}
	switch parsed.Kind {
	case "recipe":
		return recipe.Root, cleaned, nil
	case "source":
		return source.LiteralRoot, cleaned, nil
	case "workspace":
		if workspace == nil {
			return "", "", diag("missing_workspace", "@workspace ref used outside a workspace")
		}
		return workspace.Root, cleaned, nil
	case "build":
		return targetStage(source, CommandBuild, parsed.Target), cleaned, nil
	default:
		return "", "", diag("invalid_ref", "unknown package ref kind %q", parsed.Kind)
	}
}

func renderExcludesForRoot(raw []string, ctx TemplateContext, root string, source SourceState, recipe RecipeConfig, workspace *WorkspaceConfig) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	rootClean, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, item := range raw {
		rendered, err := RenderTemplate(item, ctx)
		if err != nil {
			return nil, err
		}
		exRoot, pattern, err := resolveRefRoot(rendered, "", source, recipe, workspace)
		if err != nil {
			return nil, err
		}
		exRootClean, err := filepath.Abs(exRoot)
		if err != nil {
			return nil, err
		}
		if exRootClean == rootClean {
			out = append(out, pattern)
		}
	}
	return out, nil
}

func hasGlobMagic(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

func globBase(pattern string) string {
	parts := strings.Split(filepath.ToSlash(pattern), "/")
	var base []string
	for _, part := range parts {
		if strings.ContainsAny(part, "*?[") {
			break
		}
		base = append(base, part)
	}
	return strings.Join(base, "/")
}

func excluded(path string, patterns []string) bool {
	for _, pattern := range patterns {
		ok, _ := doublestar.PathMatch(filepath.ToSlash(pattern), filepath.ToSlash(path))
		if ok {
			return true
		}
	}
	return false
}

func instanceID(inst PackageInstance) string {
	if inst.InstanceSelector == "" {
		return inst.DefinitionSelector
	}
	return inst.DefinitionSelector + ":" + inst.InstanceSelector
}

func packDeps(values map[string]string) []pack.Dependency {
	keys := sortedKeys(values)
	out := make([]pack.Dependency, 0, len(keys))
	for _, key := range keys {
		constraint := values[key]
		if constraint == "*" {
			constraint = ""
		}
		out = append(out, pack.Dependency{Name: key, Constraint: constraint})
	}
	return out
}

func packProvides(values map[string]string) []pack.Provides {
	keys := sortedKeys(values)
	out := make([]pack.Provides, 0, len(keys))
	for _, key := range keys {
		out = append(out, pack.Provides{Name: key, Version: values[key]})
	}
	return out
}

func packReplaces(values map[string]string) []pack.Replaces {
	keys := sortedKeys(values)
	out := make([]pack.Replaces, 0, len(keys))
	for _, key := range keys {
		out = append(out, pack.Replaces{Name: key, Constraint: values[key]})
	}
	return out
}

func packSDOverrides(values map[string]string) []pack.SDOverride {
	keys := sortedKeys(values)
	out := make([]pack.SDOverride, 0, len(keys))
	for _, key := range keys {
		out = append(out, pack.SDOverride{Path: key, SDDL: values[key]})
	}
	return out
}

func mustParseVersion(raw string) Version {
	v, err := ParseVersion(raw)
	if err != nil {
		return Version{Raw: raw}
	}
	return v
}

func cloneStringMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneSymlinkMap(in map[string]PackageSymlinkEntry) map[string]PackageSymlinkEntry {
	out := map[string]PackageSymlinkEntry{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneFiles(in map[string]PackageFileEntry) map[string]PackageFileEntry {
	out := map[string]PackageFileEntry{}
	for key, value := range in {
		out[key] = value
	}
	return out
}
