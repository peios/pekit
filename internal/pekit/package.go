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
	// SourcePkg marks the synthesized corresponding-source instance,
	// staged by writeSourcePackage rather than from [files] refs.
	SourcePkg bool
	// SourcePackageName is the manifest build.source_package linkage: the
	// name of the source package emitted from this recipe, empty when
	// none is.
	SourcePackageName string
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
	srcInst, err := planSourcePackage(recipe, source, instances)
	if err != nil {
		return err
	}
	if srcInst != nil {
		for i := range instances {
			if instances[i].Format == "peipkg" {
				instances[i].SourcePackageName = srcInst.Name
			}
		}
		instances = append(instances, *srcInst)
	}
	if err := validateArtifactDestinations(instances); err != nil {
		return err
	}
	if err := validateEmittedPackageNames(instances); err != nil {
		return err
	}
	signKey, err := resolveSigningKey(ctx, recipe, workspace)
	if err != nil {
		return err
	}
	run := packRun{SignKey: signKey, RecipeRef: recipeRef(recipe.Root), Builder: pekitBuilder()}
	anyPeipkg := false
	for _, inst := range instances {
		if inst.Format == "peipkg" {
			anyPeipkg = true
			break
		}
	}
	var publishOps publishPlan
	var repositoryKeys map[string]resolvedPeipkgSigningKey
	if publish {
		if source.Unanchored && !ctx.Inv.AllowUnanchored {
			return diag("unanchored_provenance", "publish from unanchored source provenance requires --allow-unanchored")
		}
		if anyPeipkg && signKey == nil && !ctx.Inv.AllowUnsigned {
			return diag("unsigned_publish", "publishing unsigned peipkg packages requires --allow-unsigned (configure %s in a keyring to sign)", signingKeyEntry)
		}
		publishOps, err = planPublishOps(workspace, recipe, instances)
		if err != nil {
			return err
		}
		if err := reservePublishDestinations(ctx, publishOps, member); err != nil {
			return err
		}
		repositoryKeys, err = resolvePeipkgSigningKeys(ctx, recipe, workspace, publishOps.Peipkg)
		if err != nil {
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
		if inst.SourcePkg {
			if err := writeSourcePackage(ctx, recipe, workspace, source, version, inst, member, run); err != nil {
				return err
			}
			continue
		}
		if err := writePackage(ctx, recipe, workspace, source, version, inst, member, run); err != nil {
			return err
		}
	}
	for _, op := range publishOps.LocalDir {
		if err := publishLocalDir(ctx, op, member); err != nil {
			return err
		}
	}
	for _, op := range publishOps.Peipkg {
		if err := publishPeipkgRepository(ctx, op, repositoryKeys[op.Dir], signKey, member); err != nil {
			return err
		}
	}
	return nil
}

func reservePublishDestinations(ctx *Context, ops publishPlan, member string) error {
	for _, op := range ops.LocalDir {
		owner := instanceID(op.Instance)
		if member != "" {
			owner = member + ":" + owner
		}
		if err := ctx.PublishRegistry.Reserve(op.Dest, owner); err != nil {
			return err
		}
	}
	for _, op := range ops.Peipkg {
		if err := ctx.PublishRegistry.ReservePeipkg(op.Dir, op.Name, op.SigningKey); err != nil {
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
	if over.Publish.Defined {
		out.Publish = PublishConfig{
			Defined:  true,
			LocalDir: append([]LocalDirPublish(nil), over.Publish.LocalDir...),
		}
		if over.Publish.Peipkg != nil {
			target := *over.Publish.Peipkg
			out.Publish.Peipkg = &target
		}
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
	if over.LicenseClass != "" {
		out.LicenseClass = over.LicenseClass
	}
	if over.Homepage != "" {
		out.Homepage = over.Homepage
	}
	if over.DefaultRoot != "" {
		out.DefaultRoot = over.DefaultRoot
	}
	// Declaring the exemption wins over a base that does not; the shared
	// workspace defaults have no business un-declaring a package's own
	// special_system_package, and "non-zero overrides" is the rule every
	// other field here follows.
	if over.SpecialSystemPackage {
		out.SpecialSystemPackage = true
	}
	// Likewise an override that declares an alternate upgrade path wins;
	// one that says nothing leaves the base's declaration in place.
	if over.AlternateUpgrade != nil {
		alt := *over.AlternateUpgrade
		out.AlternateUpgrade = &alt
	}
	// A dependency's constraint and its root are two projections of one
	// [dependencies] entry, so they replace together. Merging them as
	// independent maps let a base's root survive an override that meant
	// to drop it: a base declaring
	//
	//	libfoo = { constraint = "*", root = "initramfs" }
	//
	// overridden by the plain form `libfoo = "1.2"` set Dependencies and
	// left DependencyRoots empty, so the roots map was not replaced and
	// the base's root was applied to the new constraint. The member's
	// attempt to make it an ordinary same-root dependency silently
	// failed, and there was no way to un-set a root once a base declared
	// one. "Maps replace wholesale" is the right rule; splitting one
	// logical field across two maps that can diverge was not.
	if len(over.Dependencies) > 0 || len(over.DependencyRoots) > 0 {
		out.Dependencies = cloneStringMap(over.Dependencies)
		out.DependencyRoots = cloneStringMap(over.DependencyRoots)
	}
	if len(over.OptionalDependencies) > 0 || len(over.OptionalDependencyRoots) > 0 {
		out.OptionalDependencies = cloneStringMap(over.OptionalDependencies)
		out.OptionalDependencyRoots = cloneStringMap(over.OptionalDependencyRoots)
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
	if len(over.Claims.Provides) > 0 || len(over.Claims.Dependencies) > 0 {
		out.Claims = cloneClaims(over.Claims)
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
	renderedMeta, err := renderPackageMeta(cfg.Package, ctx)
	if err != nil {
		return PackageInstance{}, err
	}
	cfg.Package = renderedMeta
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
	}
	versionText := cfg.Package.Version
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

func renderPackageMeta(meta PackageMeta, ctx TemplateContext) (PackageMeta, error) {
	var err error
	if meta.Name, err = renderPackageString("package.name", meta.Name, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Version, err = renderPackageString("package.version", meta.Version, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Architecture, err = renderPackageString("package.architecture", meta.Architecture, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Description, err = renderPackageString("package.description", meta.Description, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.License, err = renderPackageString("package.license", meta.License, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Homepage, err = renderPackageString("package.homepage", meta.Homepage, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Dependencies, err = renderPackageStringMap("dependencies", meta.Dependencies, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.OptionalDependencies, err = renderPackageStringMap("optional_dependencies", meta.OptionalDependencies, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Conflicts, err = renderPackageStringMap("conflicts", meta.Conflicts, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Provides, err = renderPackageStringMap("provides", meta.Provides, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Replaces, err = renderPackageStringMap("replaces", meta.Replaces, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.SDOverrides, err = renderPackageStringMap("sd_overrides", meta.SDOverrides, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.SideEffects, err = renderPackageStringSlice("side_effects", meta.SideEffects, ctx); err != nil {
		return PackageMeta{}, err
	}
	if meta.Claims, err = renderClaims(meta.Claims, ctx); err != nil {
		return PackageMeta{}, err
	}
	return meta, nil
}

// cloneClaims deep-copies a ClaimsMeta so an override's claims do not
// alias the base recipe's maps.
func cloneClaims(in ClaimsMeta) ClaimsMeta {
	return ClaimsMeta{
		Provides:     cloneClaimSide(in.Provides),
		Dependencies: cloneClaimSide(in.Dependencies),
	}
}

func cloneClaimSide(in map[string]map[string]ClaimSlot) map[string]map[string]ClaimSlot {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]ClaimSlot, len(in))
	for role, slots := range in {
		cp := make(map[string]ClaimSlot, len(slots))
		for slot, s := range slots {
			cp[slot] = s
		}
		out[role] = cp
	}
	return out
}

// renderClaims runs each claim path and target through the template
// engine, so a recipe may parameterise them (e.g. an arch triplet in a
// library path) like any other manifest value.
func renderClaims(in ClaimsMeta, ctx TemplateContext) (ClaimsMeta, error) {
	var err error
	if in.Provides, err = renderClaimSide("claims.provides", in.Provides, ctx); err != nil {
		return ClaimsMeta{}, err
	}
	if in.Dependencies, err = renderClaimSide("claims.dependencies", in.Dependencies, ctx); err != nil {
		return ClaimsMeta{}, err
	}
	return in, nil
}

func renderClaimSide(field string, in map[string]map[string]ClaimSlot,
	ctx TemplateContext) (map[string]map[string]ClaimSlot, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make(map[string]map[string]ClaimSlot, len(in))
	for role, slots := range in {
		cp := make(map[string]ClaimSlot, len(slots))
		for slot, s := range slots {
			var err error
			if s.Path != "" {
				if s.Path, err = RenderTemplate(s.Path, ctx); err != nil {
					return nil, wrapDiag("template", "render "+field, err)
				}
			}
			if s.Target != "" {
				if s.Target, err = RenderTemplate(s.Target, ctx); err != nil {
					return nil, wrapDiag("template", "render "+field, err)
				}
			}
			cp[slot] = s
		}
		out[role] = cp
	}
	return out, nil
}

func renderPackageString(field, value string, ctx TemplateContext) (string, error) {
	rendered, err := RenderTemplate(value, ctx)
	if err != nil {
		return "", wrapDiag("template", "render "+field, err)
	}
	return rendered, nil
}

func renderPackageStringMap(field string, values map[string]string, ctx TemplateContext) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	seenRaw := map[string]string{}
	for _, rawKey := range sortedKeys(values) {
		key, err := renderPackageString(field+" key", rawKey, ctx)
		if err != nil {
			return nil, err
		}
		value, err := renderPackageString(field+"."+rawKey, values[rawKey], ctx)
		if err != nil {
			return nil, err
		}
		if prev, exists := seenRaw[key]; exists {
			return nil, diag("template_collision", "%s keys %q and %q render to the same name %q", field, prev, rawKey, key)
		}
		seenRaw[key] = rawKey
		out[key] = value
	}
	return out, nil
}

func renderPackageStringSlice(field string, values []string, ctx TemplateContext) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(values))
	for idx, value := range values {
		rendered, err := renderPackageString(fmt.Sprintf("%s[%d]", field, idx), value, ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, rendered)
	}
	return out, nil
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

func writePackage(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, inst PackageInstance, member string, run packRun) error {
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
		if err := writePeipkg(ctx, workspace, inst, source, entries, run); err != nil {
			return err
		}
		if run.SignKey != nil {
			ctx.Renderer.Event(Event{Type: "sign", Member: member, Package: instanceID(inst), Path: inst.Artifact, Message: "signed with key " + pack.SigningKeyFingerprint(run.SignKey)})
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
			if err := validateClaimRoles(pkg.Selector, pkg.Config.Package); err != nil {
				return err
			}
			if pkg.Config.Package.Version == "" {
				return diag("missing_package_field", "peipkg package %s requires [package].version", pkg.Selector)
			}
			if pkg.Config.Package.Architecture == "" {
				return diag("missing_package_field", "peipkg package %s requires [package].architecture", pkg.Selector)
			}
			// License is optional at the format level (PSD-009 §3.3.3) but
			// required distro-side: an unlicensed package cannot state its
			// redistribution terms, and the corresponding-source package
			// derives its own license from the members'.
			if pkg.Config.Package.License == "" {
				return diag("missing_package_field", "peipkg package %s requires [package].license", pkg.Selector)
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
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
			return diag("unsupported_payload_type", "package source %s is not a regular file, directory, or symlink", entry.Source)
		}
		seen[dest] = entry.Source
	}
	return nil
}

func validateTarPackageConfig(inst PackageInstance) error {
	meta := inst.Config.Package
	if meta.Version != "" || meta.Architecture != "" || meta.Description != "" || meta.License != "" || meta.LicenseClass != "" || meta.Homepage != "" ||
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
		matches, err = doublestar.Glob(os.DirFS(root), filepath.ToSlash(pattern), doublestar.WithNoFollow())
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
					// Non-empty directories are implied by their contents
					// (pack auto-creates ancestor directories), so they need
					// no entry of their own. An empty directory has no such
					// content to imply it, so emit it as an explicit
					// empty-directory payload entry — otherwise a pure
					// directory skeleton (e.g. fsbase's runtime mountpoints)
					// would pack to nothing.
					children, derr := os.ReadDir(path)
					if derr != nil {
						return derr
					}
					if len(children) > 0 {
						return nil
					}
				}
				rel, _ := filepath.Rel(abs, path)
				sourceRel, _ := filepath.Rel(root, path)
				sourceRel = filepath.ToSlash(sourceRel)
				if excluded(sourceRel, excludes) {
					return nil
				}
				if magic || len(matches) > 1 {
					rel = sourceRel
					if base != "" {
						if r, err := filepath.Rel(filepath.FromSlash(base), filepath.FromSlash(sourceRel)); err == nil {
							rel = filepath.ToSlash(r)
						}
					}
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
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			// POSIX pax mandates a trailing slash on directory entry names;
			// FileInfoHeader adds it but the explicit Name override drops it.
			hdr.Name += "/"
		}
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

// packAlternateUpgrade carries the recipe's alternate_upgrade table into
// the manifest (§5.18); nil stays nil so the field is absent.
func packAlternateUpgrade(alt *AlternateUpgradeMeta) *pack.AlternateUpgrade {
	if alt == nil {
		return nil
	}
	return &pack.AlternateUpgrade{Message: alt.Message}
}

func writePeipkg(ctx *Context, workspace *WorkspaceConfig, inst PackageInstance, source SourceState, entries []payloadEntry, run packRun) error {
	if inst.Version == "" {
		return diag("missing_package_field", "peipkg package %s requires [package].version", instanceID(inst))
	}
	if inst.Architecture == "" {
		return diag("missing_package_field", "peipkg package %s requires [package].architecture", instanceID(inst))
	}
	if inst.Config.Package.License == "" {
		return diag("missing_package_field", "peipkg package %s requires [package].license", instanceID(inst))
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
		if err := pack.ValidateFiles(pack.Manifest{
			Architecture:         inst.Architecture,
			SpecialSystemPackage: inst.Config.Package.SpecialSystemPackage,
		}, validateFiles); err != nil {
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
		LicenseClass:         meta.LicenseClass,
		Homepage:             meta.Homepage,
		DefaultRoot:          meta.DefaultRoot,
		SpecialSystemPackage: meta.SpecialSystemPackage,
		AlternateUpgrade:     packAlternateUpgrade(meta.AlternateUpgrade),
		Dependencies:         packDeps(meta.Dependencies, meta.DependencyRoots, meta.Claims.Dependencies),
		OptionalDependencies: packDeps(meta.OptionalDependencies, meta.OptionalDependencyRoots, meta.Claims.Dependencies),
		Conflicts:            packDeps(meta.Conflicts, nil, nil),
		Provides:             packProvides(meta.Provides, meta.Claims.Provides),
		Replaces:             packReplaces(meta.Replaces),
		SideEffects:          append([]string(nil), meta.SideEffects...),
		SDOverrides:          packSDOverrides(meta.SDOverrides),
		Build: pack.BuildInfo{
			Timestamp:     ctx.Start.UTC().Format(time.RFC3339),
			FarmID:        "local",
			SourceRef:     source.ProvenanceRef,
			SourcePackage: inst.SourcePackageName,
			RecipeRef:     run.RecipeRef,
			Builder:       run.Builder,
		},
	}
	// Derive provides/dependencies from the staged payload, on top of
	// whatever the recipe declared by hand: shared-library sonames (the
	// workspace symbol-version policy refines glibc-style sonames with a
	// version floor) and pkgconfig(...) capabilities from .pc files.
	for _, d := range []pack.DerivedDeps{
		pack.DeriveELFDeps(files, inst.Version, workspace.symbolVersionPolicy()),
		pack.DerivePkgConfigDeps(files),
	} {
		manifest.Provides = mergeProvides(manifest.Provides, d.Provides)
		manifest.Dependencies = mergeDeps(manifest.Dependencies, d.Dependencies)
		for _, w := range d.Warnings {
			fmt.Fprintf(ctx.App.Stderr, "warning: %s: %s\n", instanceID(inst), w)
		}
	}
	// A provider claim must point at a file this package ships (§4.4): keeps
	// claim targets in-root, so the materialised symlink can be relative.
	if err := pack.ValidateClaimTargets(manifest.Provides, files); err != nil {
		return wrapDiag("claim_target_validation", instanceID(inst), err)
	}
	// side_effects must agree with the payload in both directions (§5.24).
	// Checked against the full file map rather than validateFiles: an
	// override entry escapes the layout rules, but a kernel module still
	// needs indexing wherever it was declared.
	sideEffectWarnings, sideEffectErr := pack.ValidateSideEffects(manifest, files)
	for _, w := range sideEffectWarnings {
		fmt.Fprintf(ctx.App.Stderr, "warning: %s: %s\n", instanceID(inst), w)
	}
	if sideEffectErr != nil {
		return wrapDiag("side_effect_validation", instanceID(inst), sideEffectErr)
	}
	if err := pack.Pack(pack.PackOptions{Manifest: manifest, Files: files, Out: f, SignKey: run.SignKey}); err != nil {
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

type plannedPeipkgPublish struct {
	Dir        string
	Name       string
	SigningKey string
	Instances  []PackageInstance
}

type publishPlan struct {
	LocalDir []plannedPublish
	Peipkg   []plannedPeipkgPublish
}

func planPublishOps(workspace *WorkspaceConfig, recipe RecipeConfig, instances []PackageInstance) (publishPlan, error) {
	var plan publishPlan
	seen := map[string]plannedPublish{}
	repositories := map[string]int{}
	for _, inst := range instances {
		if len(inst.Config.Publish.LocalDir) == 0 && inst.Config.Publish.Peipkg == nil {
			return publishPlan{}, diag("missing_publish_target", "package %s has no publish target", inst.DefinitionSelector)
		}
		for _, target := range inst.Config.Publish.LocalDir {
			dst, overwrite, err := renderLocalDirDestination(workspace, recipe, inst, target)
			if err != nil {
				return publishPlan{}, err
			}
			if prev, ok := seen[dst]; ok {
				if prev.Instance.Artifact == inst.Artifact {
					continue
				}
				return publishPlan{}, diagAt("publish_collision", dst, "publish destination collision between %s and %s", instanceID(prev.Instance), instanceID(inst))
			}
			if !overwrite && fileExists(dst) {
				return publishPlan{}, diagAt("publish_exists", dst, "publish destination exists")
			}
			op := plannedPublish{Instance: inst, Target: target, Dest: dst, Overwrite: overwrite}
			seen[dst] = op
			plan.LocalDir = append(plan.LocalDir, op)
		}
		if target := inst.Config.Publish.Peipkg; target != nil {
			if inst.Format != "peipkg" {
				return publishPlan{}, diag("invalid_publish_target",
					"package %s uses format %q; publish.peipkg accepts only peipkg artifacts",
					inst.DefinitionSelector, inst.Format)
			}
			dir, name, err := renderPeipkgRepository(workspace, recipe, *target)
			if err != nil {
				return publishPlan{}, err
			}
			if idx, ok := repositories[dir]; ok {
				op := &plan.Peipkg[idx]
				if op.Name != name || op.SigningKey != target.SigningKey {
					return publishPlan{}, diagAt("publish_collision", dir,
						"peipkg repository target has conflicting name or signing_key settings")
				}
				duplicate := false
				for _, previous := range op.Instances {
					if previous.Artifact == inst.Artifact {
						duplicate = true
						break
					}
				}
				if !duplicate {
					op.Instances = append(op.Instances, inst)
				}
				continue
			}
			repositories[dir] = len(plan.Peipkg)
			plan.Peipkg = append(plan.Peipkg, plannedPeipkgPublish{
				Dir: dir, Name: name, SigningKey: target.SigningKey,
				Instances: []PackageInstance{inst},
			})
		}
	}
	return plan, nil
}

func renderPeipkgRepository(workspace *WorkspaceConfig, recipe RecipeConfig, target PeipkgPublish) (string, string, error) {
	base := recipe.Root
	if workspace != nil {
		base = workspace.Root
	}
	rel, err := cleanRelPath(target.Path)
	if err != nil {
		return "", "", diagAt("invalid_path", target.Path, "invalid publish.peipkg path: %v", err)
	}
	dir := filepath.Join(base, filepath.FromSlash(rel))
	name := target.Name
	if name == "" {
		name = filepath.Base(filepath.Clean(dir))
	}
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", "", diagAt("invalid_repository_name", target.Path,
			"publish.peipkg cannot derive a repository name from path; set name explicitly")
	}
	return dir, name, nil
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

// validateClaimRoles rejects a [claims.*] stanza naming a role the
// package neither provides nor depends on.
//
// packDeps and packProvides attach claims[key] only for roles that
// appear in the dependency or provides maps, so a mistyped role name
// produced no diagnostic at all — the claim simply did not exist in the
// shipped manifest. That is the same class of mistake pekit's otherwise
// strict unknown-key rejection catches everywhere else, and it slipped
// through because the key is a role name rather than a schema field
// (PEI-445).
func validateClaimRoles(selector string, meta PackageMeta) error {
	for _, role := range sortedKeys(meta.Claims.Provides) {
		if _, ok := meta.Provides[role]; !ok {
			return diag("unknown_claim_role",
				"package %s declares [claims.provides.%s] but does not provide %q",
				selector, role, role)
		}
	}
	// One claims.dependencies map serves both dependency maps, so a role
	// in either satisfies it.
	for _, role := range sortedKeys(meta.Claims.Dependencies) {
		_, req := meta.Dependencies[role]
		_, opt := meta.OptionalDependencies[role]
		if !req && !opt {
			return diag("unknown_claim_role",
				"package %s declares [claims.dependencies.%s] but does not depend on %q",
				selector, role, role)
		}
	}
	return nil
}

func packDeps(values, roots map[string]string, claims map[string]map[string]ClaimSlot) []pack.Dependency {
	keys := sortedKeys(values)
	out := make([]pack.Dependency, 0, len(keys))
	for _, key := range keys {
		constraint := values[key]
		if constraint == "*" {
			constraint = ""
		}
		out = append(out, pack.Dependency{
			Name: key, Constraint: constraint, Root: roots[key],
			Claims: packClaimSlots(claims[key])})
	}
	return out
}

func packProvides(values map[string]string, claims map[string]map[string]ClaimSlot) []pack.Provides {
	keys := sortedKeys(values)
	out := make([]pack.Provides, 0, len(keys))
	for _, key := range keys {
		out = append(out, pack.Provides{
			Name: key, Version: values[key], Claims: packClaimSlots(claims[key])})
	}
	return out
}

// mergeProvides appends derived provides to the recipe-declared set, keeping
// the recipe entry on a name collision (it may carry a version or claims).
func mergeProvides(existing, derived []pack.Provides) []pack.Provides {
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Name] = true
	}
	for _, p := range derived {
		if !have[p.Name] {
			existing = append(existing, p)
			have[p.Name] = true
		}
	}
	return existing
}

// mergeDeps appends derived dependencies to the recipe-declared set, keeping
// the recipe entry on a name collision (its explicit constraint wins).
func mergeDeps(existing, derived []pack.Dependency) []pack.Dependency {
	have := make(map[string]bool, len(existing))
	for _, d := range existing {
		have[d.Name] = true
	}
	for _, d := range derived {
		if !have[d.Name] {
			existing = append(existing, d)
			have[d.Name] = true
		}
	}
	return existing
}

// packClaimSlots adapts a recipe role's slot map to the pack form,
// returning nil when the role declares no claims.
func packClaimSlots(slots map[string]ClaimSlot) map[string]pack.ClaimSlot {
	if len(slots) == 0 {
		return nil
	}
	out := make(map[string]pack.ClaimSlot, len(slots))
	for slot, s := range slots {
		out[slot] = pack.ClaimSlot{Path: s.Path, Target: s.Target}
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
