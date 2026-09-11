package pekit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/peios/peipkg/pack"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ShellCommand struct {
	Shell string
	Args  []string
}

func (c ShellCommand) Empty() bool {
	return c.Shell == "" && len(c.Args) == 0
}

type TargetConfig struct {
	Name         string
	Kind         Command
	Command      ShellCommand
	Needs        []string
	Dependencies map[string]map[string]string
	ClearOut     bool
	// Gate makes a test target a release gate: package and publish run it
	// after staging its required builds and before writing any artifact.
	Gate bool
	// Sign holds the target's post-run signing tables, keyed by kind
	// (`sign.<kind>`), each mapping an output-relative path or glob to
	// the keyring leaf that names its private key. Build targets only.
	Sign  map[string]map[string]string
	Owner string
	Path  string

	// gen-only fields. VerifyCommand is the drift gate run by `pekit verify`
	// and by the build/test/package/publish pre-flight. VerifyOnBuild and
	// VerifyOnTest scope which targets the pre-flight gates, per namespace:
	// nil (absent) gates ALL targets in that namespace, a non-nil empty slice
	// gates NONE (verify only on demand), and a populated slice gates only the
	// listed bare target names. VerifyDependencies, when non-nil, fully
	// replaces Dependencies for the verify_command run.
	VerifyCommand      ShellCommand
	VerifyOnBuild      *[]string
	VerifyOnTest       *[]string
	VerifyDependencies map[string]map[string]string
}

type EnvVar struct {
	Name  string
	Value string
}

type RecipeConfig struct {
	Root          string
	Path          string
	OutDir        string
	Env           []EnvVar
	Wrap          ShellCommand
	Targets       map[Command]map[string]TargetConfig
	Source        SourceConfig
	Delegate      DelegateConfig
	SourcePackage SourcePackageConfig
}

// SourcePackageConfig controls the corresponding-source package a recipe
// emits alongside its binary packages. Emission defaults on for any
// recipe with a reproducible [source] that produces peipkg-format
// packages; the table exists to opt out or rename.
type SourcePackageConfig struct {
	Name    string
	Enabled *bool
}

func (c SourcePackageConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

type DelegateConfig struct {
	All      bool
	Build    bool
	Env      bool
	Wrap     bool
	Packages bool
}

func (d DelegateConfig) AllowsBuild() bool    { return d.All || d.Build }
func (d DelegateConfig) AllowsEnv() bool      { return d.All || d.Env }
func (d DelegateConfig) AllowsWrap() bool     { return d.All || d.Wrap }
func (d DelegateConfig) AllowsPackages() bool { return d.All || d.Packages }
func (d DelegateConfig) Any() bool {
	return d.All || d.Build || d.Env || d.Wrap || d.Packages
}

type SourceConfig struct {
	Git   GitSourceConfig
	URL   URLSourceConfig
	PyPI  PyPISourceConfig
	Local LocalSourceConfig
	// Patches names a recipe-root directory whose series file pekit
	// applies to the materialised source tree before any target runs.
	Patches string
}

func (s SourceConfig) HasReproducible() bool {
	return s.Git.URL != "" || s.URL.URL != "" || s.PyPI.Project != ""
}
func (s SourceConfig) HasExternal() bool { return s.HasReproducible() || s.Local.Path != "" }

type GitSourceConfig struct {
	URL         string
	Ref         string
	Versions    string
	TagRegex    string
	TrackedPath string
}

type URLSourceConfig struct {
	URL               string
	ListingURL        string
	Extract           bool
	Root              string
	Versions          string
	FileRegex         string
	Checksum          string
	ChecksumByVersion map[string]string
	Signature         URLSignatureConfig
	PatchSeries       URLPatchSeriesConfig
}

// URLPatchSeriesConfig describes an upstream-maintained, incremental patch
// series layered over a URL release archive. A selected x.y.N version uses the
// x.y base archive and patches 1 through N; enumeration discovers both the
// base releases and every contiguous patchlevel exposed by the series URL.
type URLPatchSeriesConfig struct {
	URL        string
	FileRegex  string
	PatchWidth int
	Strip      int
	Signature  URLSignatureConfig
}

func (c URLPatchSeriesConfig) Configured() bool { return c.URL != "" }

// PyPISourceConfig selects source distributions from PyPI's standardized
// JSON Simple API. Artifact is deliberately explicit even though only sdists
// are supported today, so adding another distribution kind cannot silently
// change an existing recipe's source selection.
type PyPISourceConfig struct {
	Project  string
	Artifact string
	Versions string
}

// URLSignatureConfig pins upstream release-signature verification for a URL
// source. Presence of the block makes verification required: a missing or
// invalid signature fails the fetch and nothing is locked. KeyFiles are
// committed public keys resolved relative to the recipe root; Of selects
// what the detached signature covers — the artifact as published, or its
// decompressed content (kernel.org signs the uncompressed tar). IgnoreExpiry
// permits new signatures made after a pinned key expired; it does not relax
// any other signature or trust-policy check.
type URLSignatureConfig struct {
	URL          string // template; empty means "{{source_url}}.sig"
	Of           string // "artifact" (default) or "decompressed"
	KeyFiles     []string
	Fingerprints []string
	IgnoreExpiry bool
}

func (c URLSignatureConfig) Configured() bool { return len(c.KeyFiles) > 0 }

type LocalSourceConfig struct {
	Path         string
	ResolvedPath string
}

type WorkspaceConfig struct {
	Root    string
	Path    string
	Include []string
	Exclude []string
	Env     []EnvVar
	Wrap    ShellCommand
	Policy  PolicyConfig
}

// PolicyConfig holds distro-wide derivation policy declared in the
// workspace file. SymbolVersions maps a shared-library soname to the
// symbol-version token prefix whose tokens are commensurable with the
// providing package's version (e.g. "libc.so.6" -> "GLIBC_"); it governs
// which sonames get a symbol-version floor during dependency derivation.
type PolicyConfig struct {
	SymbolVersions map[string]string
}

// symbolVersionPolicy adapts the workspace policy to the pack derivation
// API, tolerating a nil workspace (no policy configured).
func (w *WorkspaceConfig) symbolVersionPolicy() pack.SymbolVersionPolicy {
	if w == nil || len(w.Policy.SymbolVersions) == 0 {
		return nil
	}
	return pack.SymbolVersionPolicy(w.Policy.SymbolVersions)
}

type EnvFile struct {
	Path               string
	Env                []EnvVar
	Wrap               ShellCommand
	DependencyProvider string
}

type PackageLayer struct {
	Owner    string
	Root     string
	Path     string
	Selector string
	Base     bool
	Config   PackageConfig
}

type PackageConfig struct {
	Format    string
	ClearOut  *bool
	Builds    []string
	Package   PackageMeta
	Files     map[string]PackageFileEntry
	Symlinks  map[string]PackageSymlinkEntry
	Excludes  []string
	Multipack MultipackConfig
	Publish   PublishConfig
}

// AlternateUpgradeMeta is the recipe's [package] alternate_upgrade
// table (§5.18): `alternate_upgrade = { message = "..." }`, or the
// equivalent `[package.alternate_upgrade]` sub-table.
type AlternateUpgradeMeta struct {
	Message string
}

type PackageMeta struct {
	Name         string
	Version      string
	Architecture string
	Description  string
	License      string
	// LicenseClass is the §3.3.6 licence class: "unknown", "free",
	// "firmware" or "proprietary". Empty when the recipe declares none,
	// which the manifest reads as unknown — every recipe that predates
	// the field packs exactly as before.
	LicenseClass string
	Homepage     string
	// DefaultRoot is the package's top-level placement preference
	// (§3.3.6, DESIGN-named-roots.md): the named root a top-level install
	// lands in. Empty for no preference.
	DefaultRoot string
	// SpecialSystemPackage declares the package exempt from the §3.4
	// payload layout rules — fsbase's mountpoint tree, the kernel, and
	// the few others that lay down the structure those rules protect.
	// It disables pack-time layout validation for this package.
	//
	// It grants nothing at install time: the operator must also pass
	// --dangerously-bypass-path-restrictions (or the compose
	// equivalent). A recipe may propose its own exemption; only whoever
	// installs the result can grant it.
	SpecialSystemPackage bool
	// AlternateUpgrade declares that the package is meant to be installed
	// and upgraded by some means other than a routine package operation
	// (§5.18) — an operating-system edition moved by a dedicated tool,
	// for example. nil when the recipe declares none. Message is
	// required: non-empty UTF-8 of at most 1024 bytes, newlines permitted
	// and no other control character.
	//
	// It grants nothing at install time: peipkg refuses a request naming
	// the package and holds it back from an every-package upgrade unless
	// the operator passes --bypass-alternate-upgrade. peipkg-compose
	// ignores it — composing an image is not a system upgrade.
	AlternateUpgrade *AlternateUpgradeMeta

	Dependencies         map[string]string
	OptionalDependencies map[string]string
	Conflicts            map[string]string
	// DependencyRoots and OptionalDependencyRoots carry the optional
	// per-dependency placement root (§4.1.1), keyed by the same dependency
	// name as the constraint maps above. A name absent here means the
	// depender's own root (the default). They are kept parallel to the
	// constraint maps so version-constraint templating and merging are
	// unchanged; a root is a static name and is neither templated nor
	// carried for conflicts (which stay root-local).
	DependencyRoots         map[string]string
	OptionalDependencyRoots map[string]string
	Provides                map[string]string
	Replaces                map[string]string
	SideEffects             []string
	SDOverrides             map[string]string
	// Claims attaches claim slots (PSD-009 §4.4) to provides and
	// dependency entries, keyed by role. It is parsed from the top-level
	// [claims] table; recipes without claims leave it empty.
	Claims ClaimsMeta
}

// ClaimsMeta carries a package's claim declarations: the provider side
// (slots a provides entry fills) and the consumer side (slots a
// dependency expects). Each is keyed role -> slot -> descriptor (§4.4.2).
type ClaimsMeta struct {
	Provides     map[string]map[string]ClaimSlot
	Dependencies map[string]map[string]ClaimSlot
}

// ClaimSlot is one slot descriptor: a Path (the symlink location, set on
// a consumer or as a provider default) and a Target (the holder file,
// set on a provider).
type ClaimSlot struct {
	Path   string
	Target string
}

type PackageFileEntry struct {
	Source   string
	Path     string
	Override bool
}

type PackageSymlinkEntry struct {
	Target   string
	Override bool
}

type MultipackConfig struct {
	Enum      []string
	EnumFiles MultipackEnumFiles
}

type MultipackEnumFiles struct {
	Path  string
	Regex string
}

type PublishConfig struct {
	Defined  bool
	LocalDir []LocalDirPublish
	Peipkg   *PeipkgPublish
}

type LocalDirPublish struct {
	Path      string
	Overwrite *bool
}

// PeipkgPublish names a repository state directory maintained through
// peipkg/repopub. Name is used only when the directory needs initialising;
// an existing repository's signed descriptor remains authoritative.
type PeipkgPublish struct {
	Path       string
	Name       string
	SigningKey string
}

func LoadRecipe(path string) (RecipeConfig, error) {
	root := filepath.Dir(path)
	raw, md, err := loadTOMLWithMeta(path)
	if err != nil {
		return RecipeConfig{}, err
	}
	cfg := RecipeConfig{
		Root:    root,
		Path:    path,
		OutDir:  "out",
		Targets: map[Command]map[string]TargetConfig{},
	}
	known := map[string]bool{
		"out_dir": true, "env": true, "wrap": true, "source": true, "delegate": true,
		"build": true, "test": true, "install": true, "clean": true, "gen": true,
		"source_package": true,
	}
	for key := range raw {
		if !known[key] {
			return RecipeConfig{}, diagAt("unknown_key", path, "unknown recipe key %q", key)
		}
	}
	if v, ok := raw["out_dir"]; ok {
		cfg.OutDir, err = expectString(path, "out_dir", v)
		if err != nil {
			return RecipeConfig{}, err
		}
	}
	if err := validateOutDir(path, root, cfg.OutDir); err != nil {
		return RecipeConfig{}, err
	}
	if v, ok := raw["env"]; ok {
		cfg.Env, err = parseEnvVars(path, "env", v, md)
		if err != nil {
			return RecipeConfig{}, err
		}
	}
	if v, ok := raw["wrap"]; ok {
		cfg.Wrap, err = parseWrap(path, "wrap", v)
		if err != nil {
			return RecipeConfig{}, err
		}
	}
	if v, ok := raw["source"]; ok {
		cfg.Source, err = parseSource(path, root, v)
		if err != nil {
			return RecipeConfig{}, err
		}
	}
	if v, ok := raw["delegate"]; ok {
		cfg.Delegate, err = parseDelegate(path, v)
		if err != nil {
			return RecipeConfig{}, err
		}
	}
	if v, ok := raw["source_package"]; ok {
		cfg.SourcePackage, err = parseSourcePackage(path, v)
		if err != nil {
			return RecipeConfig{}, err
		}
	}
	for _, cmd := range []Command{CommandBuild, CommandTest, CommandInstall, CommandClean, CommandGen} {
		v, ok := raw[string(cmd)]
		if !ok {
			continue
		}
		targets, err := parseTargets(path, cmd, v)
		if err != nil {
			return RecipeConfig{}, err
		}
		cfg.Targets[cmd] = targets
	}
	return cfg, nil
}

// validateOutDir keeps Pekit's recursively deleted managed state strictly
// below the recipe root. Besides making recipes portable, this prevents a
// typo such as out_dir = ".", "..", or "/" from turning `pekit clean` into
// deletion of source, a workspace, or a filesystem root. A symlink at the
// resulting child path remains valid: os.RemoveAll removes the link itself,
// not the tree to which it points.
func validateOutDir(recipePath, recipeRoot, value string) error {
	if strings.TrimSpace(value) == "" {
		return diagAt("invalid_path", recipePath, "out_dir must name a directory below the recipe root")
	}
	root, err := filepath.Abs(recipeRoot)
	if err != nil {
		return wrapDiag("invalid_path", "resolve recipe root", err)
	}
	out := value
	if !filepath.IsAbs(out) {
		out = filepath.Join(root, out)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return wrapDiag("invalid_path", "resolve out_dir", err)
	}
	rel, err := filepath.Rel(root, out)
	if err != nil {
		return wrapDiag("invalid_path", "compare out_dir with recipe root", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return diagAt("invalid_path", recipePath,
			"out_dir must resolve below the recipe root, got %q", value)
	}
	return nil
}

func LoadWorkspace(path string) (WorkspaceConfig, error) {
	root := filepath.Dir(path)
	raw, md, err := loadTOMLWithMeta(path)
	if err != nil {
		return WorkspaceConfig{}, err
	}
	cfg := WorkspaceConfig{Root: root, Path: path}
	known := map[string]bool{"include": true, "exclude": true, "env": true, "wrap": true, "policy": true}
	for key := range raw {
		if !known[key] {
			return WorkspaceConfig{}, diagAt("unknown_key", path, "unknown workspace key %q", key)
		}
	}
	if v, ok := raw["include"]; ok {
		cfg.Include, err = expectStringSlice(path, "include", v)
		if err != nil {
			return WorkspaceConfig{}, err
		}
	}
	if len(cfg.Include) == 0 {
		return WorkspaceConfig{}, diagAt("missing_key", path, "workspace include is required")
	}
	if v, ok := raw["exclude"]; ok {
		cfg.Exclude, err = expectStringSlice(path, "exclude", v)
		if err != nil {
			return WorkspaceConfig{}, err
		}
	}
	if v, ok := raw["env"]; ok {
		cfg.Env, err = parseEnvVars(path, "env", v, md)
		if err != nil {
			return WorkspaceConfig{}, err
		}
	}
	if v, ok := raw["wrap"]; ok {
		cfg.Wrap, err = parseWrap(path, "wrap", v)
		if err != nil {
			return WorkspaceConfig{}, err
		}
	}
	if v, ok := raw["policy"]; ok {
		cfg.Policy, err = parsePolicy(path, v)
		if err != nil {
			return WorkspaceConfig{}, err
		}
	}
	return cfg, nil
}

// parsePolicy parses the top-level [policy] table. Currently it carries one
// sub-table, [policy.symbol_versions] (soname -> token prefix).
func parsePolicy(path string, value any) (PolicyConfig, error) {
	table, err := expectMap(path, "policy", value)
	if err != nil {
		return PolicyConfig{}, err
	}
	var out PolicyConfig
	for key, raw := range table {
		switch key {
		case "symbol_versions":
			sv, err := expectMap(path, "policy.symbol_versions", raw)
			if err != nil {
				return PolicyConfig{}, err
			}
			out.SymbolVersions = make(map[string]string, len(sv))
			for soname, prefixVal := range sv {
				prefix, err := expectString(path, "policy.symbol_versions."+soname, prefixVal)
				if err != nil {
					return PolicyConfig{}, err
				}
				out.SymbolVersions[soname] = prefix
			}
		default:
			return PolicyConfig{}, diagAt("unknown_key", path,
				"policy.%s is not a known policy table", key)
		}
	}
	return out, nil
}

func LoadEnvFile(path string, missingOK bool) (EnvFile, error) {
	if !fileExists(path) {
		if missingOK {
			return EnvFile{Path: path}, nil
		}
		return EnvFile{}, diagAt("missing_env_file", path, "env file does not exist")
	}
	raw, md, err := loadTOMLWithMeta(path)
	if err != nil {
		return EnvFile{}, err
	}
	env := EnvFile{Path: path}
	known := map[string]bool{"env": true, "wrap": true, "dependency_provider": true}
	for key := range raw {
		if !known[key] {
			return EnvFile{}, diagAt("unknown_key", path, "unknown env-file key %q", key)
		}
	}
	if _, hasEnv := raw["env"]; !hasEnv {
		if _, hasWrap := raw["wrap"]; !hasWrap {
			if _, hasProvider := raw["dependency_provider"]; !hasProvider {
				return EnvFile{}, diagAt("missing_key", path, "env file requires [env], [wrap], dependency_provider, or a combination")
			}
		}
	}
	if v, ok := raw["env"]; ok {
		env.Env, err = parseEnvVars(path, "env", v, md)
		if err != nil {
			return EnvFile{}, err
		}
	}
	if v, ok := raw["wrap"]; ok {
		env.Wrap, err = parseWrap(path, "wrap", v)
		if err != nil {
			return EnvFile{}, err
		}
	}
	if v, ok := raw["dependency_provider"]; ok {
		env.DependencyProvider, err = expectString(path, "dependency_provider", v)
		if err != nil {
			return EnvFile{}, err
		}
		if err := validateSelector("dependency provider", env.DependencyProvider); err != nil {
			return EnvFile{}, err
		}
	}
	return env, nil
}

func LoadPackageLayers(root, owner string) ([]PackageLayer, error) {
	var layers []PackageLayer
	basePaths := []string{
		filepath.Join(root, "package.pekit.toml"),
		filepath.Join(root, "packages.pekit", "package.pekit.toml"),
	}
	loadedBase := ""
	for _, path := range basePaths {
		if fileExists(path) {
			if loadedBase != "" {
				return nil, diagAt("duplicate_package_base", path, "base package file already defined at %s", loadedBase)
			}
			cfg, err := LoadPackageFile(path)
			if err != nil {
				return nil, err
			}
			cfg = normalizePackageRefs(cfg, owner)
			layers = append(layers, PackageLayer{Owner: owner, Root: root, Path: path, Selector: "main", Base: true, Config: cfg})
			loadedBase = path
		}
	}
	seen := map[string]string{}
	memberPatterns := []string{
		filepath.Join(root, "*.package.pekit.toml"),
		filepath.Join(root, "packages.pekit", "*.package.pekit.toml"),
	}
	for _, pattern := range memberPatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, wrapDiag("glob_error", pattern, err)
		}
		sortStrings(matches)
		for _, path := range matches {
			name := strings.TrimSuffix(filepath.Base(path), ".package.pekit.toml")
			if name == "" || name == "package" {
				continue
			}
			if err := validateSelector("package", name); err != nil {
				return nil, err
			}
			if prev, dup := seen[name]; dup {
				return nil, diagAt("duplicate_package", path, "package selector %q already defined at %s", name, prev)
			}
			seen[name] = path
			cfg, err := LoadPackageFile(path)
			if err != nil {
				return nil, err
			}
			cfg = normalizePackageRefs(cfg, owner)
			layers = append(layers, PackageLayer{Owner: owner, Root: root, Path: path, Selector: name, Config: cfg})
		}
	}
	return layers, nil
}

func normalizePackageRefs(cfg PackageConfig, owner string) PackageConfig {
	prefix := "@recipe:"
	switch owner {
	case "workspace":
		prefix = "@workspace:"
	case "source":
		prefix = "@source:"
	}
	if len(cfg.Files) > 0 {
		normalized := map[string]PackageFileEntry{}
		for key, entry := range cfg.Files {
			newKey := normalizePackageRefKey(key, prefix)
			entry.Source = newKey
			normalized[newKey] = entry
		}
		cfg.Files = normalized
	}
	if cfg.Multipack.EnumFiles.Path != "" {
		cfg.Multipack.EnumFiles.Path = normalizePackageRefKey(cfg.Multipack.EnumFiles.Path, prefix)
	}
	for idx, exclude := range cfg.Excludes {
		cfg.Excludes[idx] = normalizePackageRefKey(exclude, prefix)
	}
	return cfg
}

func normalizePackageRefKey(ref, prefix string) string {
	if strings.HasPrefix(ref, "@recipe:") || strings.HasPrefix(ref, "@source:") || strings.HasPrefix(ref, "@workspace:") {
		return ref
	}
	if _, _, ok := strings.Cut(ref, ":"); ok {
		return ref
	}
	return prefix + ref
}

func LoadPackageFile(path string) (PackageConfig, error) {
	raw, err := loadTOML(path)
	if err != nil {
		return PackageConfig{}, err
	}
	cfg := PackageConfig{Files: map[string]PackageFileEntry{}, Symlinks: map[string]PackageSymlinkEntry{}}
	known := map[string]bool{
		"format": true, "clear_out": true, "builds": true, "package": true, "files": true, "symlinks": true,
		"excludes": true, "multipack": true, "publish": true, "dependencies": true, "optional_dependencies": true,
		"conflicts": true, "provides": true, "replaces": true, "side_effects": true, "sd_overrides": true,
		"claims": true,
	}
	for key := range raw {
		if !known[key] {
			return PackageConfig{}, diagAt("unknown_key", path, "unknown package key %q", key)
		}
	}
	if v, ok := raw["format"]; ok {
		cfg.Format, err = expectString(path, "format", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["clear_out"]; ok {
		b, err := expectBool(path, "clear_out", v)
		if err != nil {
			return PackageConfig{}, err
		}
		cfg.ClearOut = &b
	}
	if v, ok := raw["builds"]; ok {
		cfg.Builds, err = expectStringSlice(path, "builds", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["package"]; ok {
		cfg.Package, err = parsePackageMeta(path, v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["dependencies"]; ok {
		cfg.Package.Dependencies, cfg.Package.DependencyRoots, err =
			parseDependencyMap(path, "dependencies", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["optional_dependencies"]; ok {
		cfg.Package.OptionalDependencies, cfg.Package.OptionalDependencyRoots, err =
			parseDependencyMap(path, "optional_dependencies", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["conflicts"]; ok {
		cfg.Package.Conflicts, err = parseStringMap(path, "conflicts", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["provides"]; ok {
		cfg.Package.Provides, err = parseStringMap(path, "provides", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["replaces"]; ok {
		cfg.Package.Replaces, err = parseStringMap(path, "replaces", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["side_effects"]; ok {
		cfg.Package.SideEffects, err = expectStringSlice(path, "side_effects", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["claims"]; ok {
		cfg.Package.Claims, err = parseClaims(path, v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["sd_overrides"]; ok {
		cfg.Package.SDOverrides, err = parseStringMap(path, "sd_overrides", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["files"]; ok {
		cfg.Files, err = parsePackageFiles(path, v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["symlinks"]; ok {
		cfg.Symlinks, err = parsePackageSymlinks(path, v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["excludes"]; ok {
		cfg.Excludes, err = expectStringSlice(path, "excludes", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["multipack"]; ok {
		cfg.Multipack, err = parseMultipack(path, v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["publish"]; ok {
		cfg.Publish, err = parsePublish(path, v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	return cfg, nil
}

// parseClaims parses the top-level [claims] table (§4.4.2): a provides
// and/or dependencies side, each keyed role -> slot -> {path, target}.
func parseClaims(path string, value any) (ClaimsMeta, error) {
	table, err := expectMap(path, "claims", value)
	if err != nil {
		return ClaimsMeta{}, err
	}
	var out ClaimsMeta
	for side, raw := range table {
		roles, err := parseClaimSide(path, "claims."+side, raw, side == "provides")
		if err != nil {
			return ClaimsMeta{}, err
		}
		switch side {
		case "provides":
			out.Provides = roles
		case "dependencies":
			out.Dependencies = roles
		default:
			return ClaimsMeta{}, diagAt("unknown_key", path,
				"claims.%s must be provides or dependencies", side)
		}
	}
	return out, nil
}

// parseClaimSide parses one side of [claims]: role -> slot -> descriptor.
//
// provider decides which fields a slot may carry. §5.23 makes the two
// sides asymmetric — a provider names a target, a consumer names a path
// — and peipkg enforces that at install. Accepting the wrong shape here
// meant a successful build, a signed package, and a failure on somebody
// else's machine (PEI-445).
func parseClaimSide(path, key string, value any, provider bool) (
	map[string]map[string]ClaimSlot, error) {
	roles, err := expectMap(path, key, value)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]ClaimSlot, len(roles))
	for role, rawSlots := range roles {
		slots, err := expectMap(path, key+"."+role, rawSlots)
		if err != nil {
			return nil, err
		}
		slotMap := make(map[string]ClaimSlot, len(slots))
		for slot, rawSlot := range slots {
			fields, err := expectMap(path, key+"."+role+"."+slot, rawSlot)
			if err != nil {
				return nil, err
			}
			var cs ClaimSlot
			for fk, fv := range fields {
				s, err := expectString(path, key+"."+role+"."+slot+"."+fk, fv)
				if err != nil {
					return nil, err
				}
				switch fk {
				case "path":
					cs.Path = s
				case "target":
					cs.Target = s
				default:
					return nil, diagAt("unknown_key", path,
						"%s.%s.%s.%s must be path or target", key, role, slot, fk)
				}
			}
			label := key + "." + role + "." + slot
			if provider {
				if cs.Target == "" {
					return nil, diagAt("invalid_claim", path,
						"%s is missing target: a provides claim names the path this package "+
							"ships (§5.23)", label)
				}
			} else {
				if cs.Target != "" {
					return nil, diagAt("invalid_claim", path,
						"%s sets target, which only a provides claim may set (§5.23)", label)
				}
				if cs.Path == "" {
					return nil, diagAt("invalid_claim", path,
						"%s is missing path: a dependencies claim names the well-known path "+
							"the role is reached at (§5.23)", label)
				}
			}
			slotMap[slot] = cs
		}
		out[role] = slotMap
	}
	return out, nil
}

func loadTOML(path string) (map[string]any, error) {
	raw, _, err := loadTOMLWithMeta(path)
	return raw, err
}

func loadTOMLWithMeta(path string) (map[string]any, toml.MetaData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, toml.MetaData{}, wrapDiag("read_file", path, err)
	}
	var raw map[string]any
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, toml.MetaData{}, wrapDiag("parse_toml", path, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, md, nil
}

func parseTargets(path string, kind Command, value any) (map[string]TargetConfig, error) {
	table, err := expectMap(path, string(kind), value)
	if err != nil {
		return nil, err
	}
	hasCommand := false
	for key := range table {
		if key == "command" {
			hasCommand = true
			break
		}
	}
	if hasCommand {
		for key, v := range table {
			if _, ok := v.(map[string]any); ok && !targetConfigKey(kind, key) {
				return nil, diagAt("mixed_targets", path, "cannot mix bare [%s] target with named [%s.%s]", kind, kind, key)
			}
		}
		t, err := parseTarget(path, kind, "main", table)
		if err != nil {
			return nil, err
		}
		return map[string]TargetConfig{"main": t}, nil
	}
	out := map[string]TargetConfig{}
	for name, v := range table {
		if err := validateSelector("target", name); err != nil {
			return nil, err
		}
		sub, err := expectMap(path, string(kind)+"."+name, v)
		if err != nil {
			return nil, err
		}
		t, err := parseTarget(path, kind, name, sub)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

func targetConfigKey(kind Command, key string) bool {
	if kind == CommandGen {
		switch key {
		case "command", "verify_command", "verify_on_build", "verify_on_test", "dependencies", "verify_dependencies":
			return true
		default:
			return false
		}
	}
	switch key {
	case "command", "needs", "clear_out":
		return true
	case "gate":
		return kind == CommandTest
	case "dependencies":
		// A test stage runs in a composed root just as a build does, and
		// under --env peipkg that root holds nothing the stage does not
		// name — not even a shell (PEI-489).
		return kind == CommandBuild || kind == CommandTest
	case "sign":
		return kind == CommandBuild
	default:
		return false
	}
}

func parseTarget(path string, kind Command, name string, table map[string]any) (TargetConfig, error) {
	if kind == CommandGen {
		return parseGenTarget(path, name, table)
	}
	known := map[string]bool{"command": true, "needs": true, "clear_out": true}
	if kind == CommandBuild || kind == CommandTest {
		known["dependencies"] = true
	}
	if kind == CommandTest {
		known["gate"] = true
	}
	if kind == CommandBuild {
		known["sign"] = true
	}
	for key := range table {
		if !known[key] {
			return TargetConfig{}, diagAt("unknown_key", path, "unknown target key %s.%s.%s", kind, name, key)
		}
	}
	raw, ok := table["command"]
	if !ok {
		return TargetConfig{}, diagAt("missing_key", path, "%s.%s requires command", kind, name)
	}
	cmd, err := parseCommand(path, string(kind)+"."+name+".command", raw)
	if err != nil {
		return TargetConfig{}, err
	}
	t := TargetConfig{Name: name, Kind: kind, Command: cmd, ClearOut: true, Owner: "recipe", Path: path}
	if v, ok := table["needs"]; ok {
		t.Needs, err = expectStringSlice(path, string(kind)+"."+name+".needs", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if v, ok := table["gate"]; ok {
		t.Gate, err = expectBool(path, string(kind)+"."+name+".gate", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if v, ok := table["dependencies"]; ok {
		t.Dependencies, err = parseTargetDependencies(path, string(kind)+"."+name+".dependencies", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if v, ok := table["clear_out"]; ok {
		t.ClearOut, err = expectBool(path, string(kind)+"."+name+".clear_out", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if v, ok := table["sign"]; ok {
		t.Sign, err = parseTargetSign(path, string(kind)+"."+name+".sign", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	return t, nil
}

// signKinds lists the signature formats `sign.<kind>` accepts.
var signKinds = map[string]bool{signKindPIP: true}

// parseTargetSign parses a build target's `sign` table:
//
//	[build.main.sign.pip]
//	"bin/peinit" = "tcb.priv"
//
// Each kind is a table mapping an output-relative path or glob to the
// dotted keyring leaf that names the signing key. The kind decides the
// signature format and where it is stored; the keys are looked up
// through the ordinary keyring mechanism at run time.
func parseTargetSign(path, prefix string, value any) (map[string]map[string]string, error) {
	table, ok := value.(map[string]any)
	if !ok {
		return nil, diagAt("invalid_type", path, "%s must be a table", prefix)
	}
	out := map[string]map[string]string{}
	for kind, raw := range table {
		if !signKinds[kind] {
			return nil, diagAt("unknown_key", path, "unknown signature kind %s.%s (known: %s)", prefix, kind, strings.Join(sortedKeys(signKinds), ", "))
		}
		entries, ok := raw.(map[string]any)
		if !ok {
			return nil, diagAt("invalid_type", path, "%s.%s must be a table of path = keyring entry", prefix, kind)
		}
		rules := map[string]string{}
		for pattern, v := range entries {
			key := prefix + "." + kind + "." + pattern
			entry, err := expectString(path, key, v)
			if err != nil {
				return nil, err
			}
			if _, err := cleanRelPath(pattern); err != nil {
				return nil, diagAt("invalid_path", path, "%s: %v", key, err)
			}
			if strings.TrimSpace(entry) == "" {
				return nil, diagAt("invalid_value", path, "%s must name a keyring entry", key)
			}
			rules[pattern] = entry
		}
		if len(rules) == 0 {
			return nil, diagAt("invalid_value", path, "%s.%s is empty", prefix, kind)
		}
		out[kind] = rules
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseGenTarget parses a [gen.NAME] target. Unlike build/test targets it has
// no needs (no build DAG — it operates on the committed tree) and no clear_out
// (its $PEKIT_OUT is a pekit-managed scratch dir), but it adds the drift-gate
// fields: verify_command, verify_on_{build,test}, and verify_dependencies.
func parseGenTarget(path, name string, table map[string]any) (TargetConfig, error) {
	prefix := "gen." + name
	known := map[string]bool{
		"command": true, "verify_command": true,
		"verify_on_build": true, "verify_on_test": true,
		"dependencies": true, "verify_dependencies": true,
	}
	for key := range table {
		if !known[key] {
			return TargetConfig{}, diagAt("unknown_key", path, "unknown target key %s.%s", prefix, key)
		}
	}
	raw, ok := table["command"]
	if !ok {
		return TargetConfig{}, diagAt("missing_key", path, "%s requires command", prefix)
	}
	cmd, err := parseCommand(path, prefix+".command", raw)
	if err != nil {
		return TargetConfig{}, err
	}
	t := TargetConfig{Name: name, Kind: CommandGen, Command: cmd, Owner: "recipe", Path: path}
	if v, ok := table["verify_command"]; ok {
		t.VerifyCommand, err = parseCommand(path, prefix+".verify_command", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if v, ok := table["verify_on_build"]; ok {
		names, err := parseVerifyScope(path, prefix+".verify_on_build", v)
		if err != nil {
			return TargetConfig{}, err
		}
		t.VerifyOnBuild = &names
	}
	if v, ok := table["verify_on_test"]; ok {
		names, err := parseVerifyScope(path, prefix+".verify_on_test", v)
		if err != nil {
			return TargetConfig{}, err
		}
		t.VerifyOnTest = &names
	}
	if v, ok := table["dependencies"]; ok {
		t.Dependencies, err = parseTargetDependencies(path, prefix+".dependencies", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if v, ok := table["verify_dependencies"]; ok {
		t.VerifyDependencies, err = parseTargetDependencies(path, prefix+".verify_dependencies", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	if (t.VerifyOnBuild != nil || t.VerifyOnTest != nil) && t.VerifyCommand.Empty() {
		return TargetConfig{}, diagAt("invalid_gen", path, "%s sets verify_on_build/verify_on_test but has no verify_command to gate", prefix)
	}
	if t.VerifyDependencies != nil && t.VerifyCommand.Empty() {
		return TargetConfig{}, diagAt("invalid_gen", path, "%s sets verify_dependencies but has no verify_command", prefix)
	}
	return t, nil
}

// parseVerifyScope parses a verify_on_build / verify_on_test array. An empty
// array is valid and meaningful (gate nothing); each entry must be a canonical
// bare target name within the referenced namespace.
func parseVerifyScope(path, key string, value any) ([]string, error) {
	names, err := expectStringSlice(path, key, value)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if err := validateSelector("target", n); err != nil {
			return nil, diagAt("invalid_selector", path, "%s: %s", key, err.Error())
		}
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

func parseTargetDependencies(path, key string, value any) (map[string]map[string]string, error) {
	table, err := expectMap(path, key, value)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for provider, raw := range table {
		if err := validateSelector("dependency provider", provider); err != nil {
			return nil, err
		}
		depTable, err := expectMap(path, key+"."+provider, raw)
		if err != nil {
			return nil, err
		}
		deps := map[string]string{}
		for name, depRaw := range depTable {
			// Dependency names may be real package names or virtual
			// (capability) names — sonames, pkgconfig(...), etc. Use the
			// same grammar peipkg enforces at manifest decode.
			if err := pack.ValidateCapabilityName(name); err != nil {
				return nil, diagAt("invalid_dependency", path, "%s.%s dependency name %q is not valid: %v", key, provider, name, err)
			}
			constraint, ok := depRaw.(string)
			if !ok {
				return nil, diagAt("invalid_type", path, "%s.%s.%s must be a string", key, provider, name)
			}
			if strings.TrimSpace(constraint) == "" {
				return nil, diagAt("invalid_dependency", path, "%s.%s.%s constraint must be non-empty; use \"*\" for any version", key, provider, name)
			}
			deps[name] = constraint
		}
		out[provider] = deps
	}
	return out, nil
}

func parseSource(path, root string, value any) (SourceConfig, error) {
	table, err := expectMap(path, "source", value)
	if err != nil {
		return SourceConfig{}, err
	}
	cfg := SourceConfig{}
	known := map[string]bool{"git": true, "url": true, "pypi": true, "local": true, "patches": true}
	for key := range table {
		if !known[key] {
			return SourceConfig{}, diagAt("unknown_key", path, "unknown source key %q; v2 uses [source.git], [source.url], [source.pypi], and [source.local]", key)
		}
	}
	if v, ok := table["patches"]; ok {
		patches, err := expectString(path, "source.patches", v)
		if err != nil {
			return SourceConfig{}, err
		}
		// A single directory name: the whole directory ships as the source
		// package's patches/, so nesting or escaping is not allowed.
		if patches == "" || patches != filepath.Base(patches) || patches == "." || patches == ".." {
			return SourceConfig{}, diagAt("invalid_path", path, "source.patches must name a directory in the recipe root, got %q", patches)
		}
		cfg.Patches = patches
	}
	repro := 0
	if v, ok := table["git"]; ok {
		repro++
		gitTable, err := expectMap(path, "source.git", v)
		if err != nil {
			return SourceConfig{}, err
		}
		cfg.Git, err = parseGitSource(path, gitTable)
		if err != nil {
			return SourceConfig{}, err
		}
	}
	if v, ok := table["url"]; ok {
		repro++
		urlTable, err := expectMap(path, "source.url", v)
		if err != nil {
			return SourceConfig{}, err
		}
		cfg.URL, err = parseURLSource(path, urlTable)
		if err != nil {
			return SourceConfig{}, err
		}
	}
	if v, ok := table["pypi"]; ok {
		repro++
		pypiTable, err := expectMap(path, "source.pypi", v)
		if err != nil {
			return SourceConfig{}, err
		}
		cfg.PyPI, err = parsePyPISource(path, pypiTable)
		if err != nil {
			return SourceConfig{}, err
		}
	}
	if repro > 1 {
		return SourceConfig{}, diagAt("mixed_source", path, "exactly one reproducible source table is allowed")
	}
	if v, ok := table["local"]; ok {
		localTable, err := expectMap(path, "source.local", v)
		if err != nil {
			return SourceConfig{}, err
		}
		cfg.Local, err = parseLocalSource(path, root, localTable)
		if err != nil {
			return SourceConfig{}, err
		}
	}
	if cfg.Patches != "" && !cfg.HasReproducible() {
		return SourceConfig{}, diagAt("patches_source", path, "source.patches requires [source.git], [source.url], or [source.pypi]")
	}
	return cfg, nil
}

func parsePyPISource(path string, table map[string]any) (PyPISourceConfig, error) {
	known := map[string]bool{"project": true, "artifact": true, "versions": true}
	for key := range table {
		if !known[key] {
			return PyPISourceConfig{}, diagAt("unknown_key", path, "unknown source.pypi key %q", key)
		}
	}
	project, err := requiredString(path, "source.pypi.project", table)
	if err != nil {
		return PyPISourceConfig{}, err
	}
	if !pypiProjectNameRE.MatchString(project) {
		return PyPISourceConfig{}, diagAt("invalid_pypi_project", path,
			"source.pypi.project %q must start and end with an ASCII letter or digit and contain only letters, digits, '.', '_', or '-'", project)
	}
	artifact, err := requiredString(path, "source.pypi.artifact", table)
	if err != nil {
		return PyPISourceConfig{}, err
	}
	if artifact != "sdist" {
		return PyPISourceConfig{}, diagAt("invalid_pypi_artifact", path,
			"source.pypi.artifact must be \"sdist\", got %q", artifact)
	}
	cfg := PyPISourceConfig{Project: project, Artifact: artifact}
	if v, ok := table["versions"]; ok {
		cfg.Versions, err = expectString(path, "source.pypi.versions", v)
		if err != nil {
			return PyPISourceConfig{}, err
		}
	}
	return cfg, nil
}

func parseGitSource(path string, table map[string]any) (GitSourceConfig, error) {
	known := map[string]bool{"url": true, "ref": true, "versions": true, "tag_regex": true, "tracked_path": true}
	for key := range table {
		if !known[key] {
			return GitSourceConfig{}, diagAt("unknown_key", path, "unknown source.git key %q", key)
		}
	}
	url, err := requiredString(path, "source.git.url", table)
	if err != nil {
		return GitSourceConfig{}, err
	}
	cfg := GitSourceConfig{URL: url, Ref: "{{version}}"}
	if v, ok := table["ref"]; ok {
		cfg.Ref, err = expectString(path, "source.git.ref", v)
		if err != nil {
			return GitSourceConfig{}, err
		}
	}
	if v, ok := table["versions"]; ok {
		cfg.Versions, err = expectString(path, "source.git.versions", v)
		if err != nil {
			return GitSourceConfig{}, err
		}
	}
	if v, ok := table["tag_regex"]; ok {
		cfg.TagRegex, err = expectString(path, "source.git.tag_regex", v)
		if err != nil {
			return GitSourceConfig{}, err
		}
	}
	if v, ok := table["tracked_path"]; ok {
		cfg.TrackedPath, err = expectString(path, "source.git.tracked_path", v)
		if err != nil {
			return GitSourceConfig{}, err
		}
		cfg.TrackedPath, err = cleanRelPath(cfg.TrackedPath)
		if err != nil || cfg.TrackedPath == "." {
			return GitSourceConfig{}, diagAt("invalid_path", path, "source.git.tracked_path must name one relative file within the repository")
		}
		if strings.Contains(cfg.Ref, "{{") || strings.Contains(cfg.Ref, "}}") {
			return GitSourceConfig{}, diagAt("invalid_value", path, "source.git.tracked_path requires a fixed, non-templated source.git.ref")
		}
		if strings.TrimSpace(cfg.Ref) == "" || strings.HasPrefix(cfg.Ref, "-") || strings.ContainsAny(cfg.Ref, ":\r\n") {
			return GitSourceConfig{}, diagAt("invalid_value", path, "source.git.tracked_path requires one fixed ref name, not an option or refspec")
		}
		if cfg.TagRegex != "" {
			return GitSourceConfig{}, diagAt("invalid_value", path, "source.git.tag_regex cannot be combined with source.git.tracked_path")
		}
	}
	return cfg, nil
}

func parseURLSource(path string, table map[string]any) (URLSourceConfig, error) {
	known := map[string]bool{"url": true, "listing_url": true, "extract": true, "root": true, "versions": true, "file_regex": true, "checksum": true, "signature": true, "patch_series": true}
	for key := range table {
		if !known[key] {
			return URLSourceConfig{}, diagAt("unknown_key", path, "unknown source.url key %q", key)
		}
	}
	url, err := requiredString(path, "source.url.url", table)
	if err != nil {
		return URLSourceConfig{}, err
	}
	cfg := URLSourceConfig{URL: url, Root: ".", ChecksumByVersion: map[string]string{}}
	if v, ok := table["listing_url"]; ok {
		cfg.ListingURL, err = expectString(path, "source.url.listing_url", v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	if v, ok := table["extract"]; ok {
		cfg.Extract, err = expectBool(path, "source.url.extract", v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	if v, ok := table["root"]; ok {
		cfg.Root, err = expectString(path, "source.url.root", v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	if v, ok := table["versions"]; ok {
		cfg.Versions, err = expectString(path, "source.url.versions", v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	if v, ok := table["file_regex"]; ok {
		cfg.FileRegex, err = expectString(path, "source.url.file_regex", v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	if v, ok := table["checksum"]; ok {
		switch typed := v.(type) {
		case string:
			cfg.Checksum = typed
		default:
			cfg.ChecksumByVersion, err = parseStringMap(path, "source.url.checksum", v)
			if err != nil {
				return URLSourceConfig{}, err
			}
		}
	}
	if v, ok := table["signature"]; ok {
		cfg.Signature, err = parseURLSignature(path, "source.url.signature", v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	if v, ok := table["patch_series"]; ok {
		cfg.PatchSeries, err = parseURLPatchSeries(path, v)
		if err != nil {
			return URLSourceConfig{}, err
		}
	}
	return cfg, nil
}

func parseURLPatchSeries(path string, value any) (URLPatchSeriesConfig, error) {
	table, err := expectMap(path, "source.url.patch_series", value)
	if err != nil {
		return URLPatchSeriesConfig{}, err
	}
	known := map[string]bool{"url": true, "file_regex": true, "patch_width": true, "strip": true, "signature": true}
	for key := range table {
		if !known[key] {
			return URLPatchSeriesConfig{}, diagAt("unknown_key", path, "unknown source.url.patch_series key %q", key)
		}
	}
	url, err := requiredString(path, "source.url.patch_series.url", table)
	if err != nil {
		return URLPatchSeriesConfig{}, err
	}
	if !strings.Contains(url, "{{patch}}") {
		return URLPatchSeriesConfig{}, diagAt("invalid_patch_series", path, "source.url.patch_series.url must contain {{patch}}")
	}
	cfg := URLPatchSeriesConfig{URL: url}
	if v, ok := table["file_regex"]; ok {
		cfg.FileRegex, err = expectString(path, "source.url.patch_series.file_regex", v)
		if err != nil {
			return URLPatchSeriesConfig{}, err
		}
		if re, compileErr := regexp.Compile(cfg.FileRegex); compileErr != nil {
			return URLPatchSeriesConfig{}, wrapDiag("invalid_regex", "source.url.patch_series.file_regex", compileErr)
		} else if re.SubexpIndex("patch") < 0 {
			return URLPatchSeriesConfig{}, diagAt("invalid_patch_series", path, "source.url.patch_series.file_regex must contain a named patch capture")
		}
	}
	if v, ok := table["patch_width"]; ok {
		cfg.PatchWidth, err = expectNonnegativeInt(path, "source.url.patch_series.patch_width", v)
		if err != nil {
			return URLPatchSeriesConfig{}, err
		}
	}
	if v, ok := table["strip"]; ok {
		cfg.Strip, err = expectNonnegativeInt(path, "source.url.patch_series.strip", v)
		if err != nil {
			return URLPatchSeriesConfig{}, err
		}
	}
	if v, ok := table["signature"]; ok {
		cfg.Signature, err = parseURLSignature(path, "source.url.patch_series.signature", v)
		if err != nil {
			return URLPatchSeriesConfig{}, err
		}
	}
	return cfg, nil
}

func parseURLSignature(path, field string, value any) (URLSignatureConfig, error) {
	table, err := expectMap(path, field, value)
	if err != nil {
		return URLSignatureConfig{}, err
	}
	known := map[string]bool{"url": true, "of": true, "key_files": true, "fingerprints": true, "ignore_expiry": true}
	for key := range table {
		if !known[key] {
			return URLSignatureConfig{}, diagAt("unknown_key", path, "unknown %s key %q", field, key)
		}
	}
	cfg := URLSignatureConfig{}
	if v, ok := table["url"]; ok {
		cfg.URL, err = expectString(path, field+".url", v)
		if err != nil {
			return URLSignatureConfig{}, err
		}
	}
	if v, ok := table["of"]; ok {
		cfg.Of, err = expectString(path, field+".of", v)
		if err != nil {
			return URLSignatureConfig{}, err
		}
		if cfg.Of != "artifact" && cfg.Of != "decompressed" {
			return URLSignatureConfig{}, diagAt("invalid_signature", path, "%s.of must be \"artifact\" or \"decompressed\"", field)
		}
	}
	if v, ok := table["key_files"]; ok {
		cfg.KeyFiles, err = expectStringSlice(path, field+".key_files", v)
		if err != nil {
			return URLSignatureConfig{}, err
		}
	}
	if len(cfg.KeyFiles) == 0 {
		return URLSignatureConfig{}, diagAt("missing_key", path, "%s requires a non-empty key_files", field)
	}
	if v, ok := table["fingerprints"]; ok {
		cfg.Fingerprints, err = expectStringSlice(path, field+".fingerprints", v)
		if err != nil {
			return URLSignatureConfig{}, err
		}
	}
	if v, ok := table["ignore_expiry"]; ok {
		cfg.IgnoreExpiry, err = expectBool(path, field+".ignore_expiry", v)
		if err != nil {
			return URLSignatureConfig{}, err
		}
	}
	return cfg, nil
}

func parseLocalSource(path, root string, table map[string]any) (LocalSourceConfig, error) {
	known := map[string]bool{"path": true}
	for key := range table {
		if !known[key] {
			return LocalSourceConfig{}, diagAt("unknown_key", path, "unknown source.local key %q", key)
		}
	}
	raw, err := requiredString(path, "source.local.path", table)
	if err != nil {
		return LocalSourceConfig{}, err
	}
	resolved, err := absPath(root, raw)
	if err != nil {
		return LocalSourceConfig{}, wrapDiag("invalid_path", "resolve source.local.path", err)
	}
	return LocalSourceConfig{Path: raw, ResolvedPath: resolved}, nil
}

func parseDelegate(path string, value any) (DelegateConfig, error) {
	switch v := value.(type) {
	case bool:
		return DelegateConfig{All: v}, nil
	default:
		table, err := expectMap(path, "delegate", value)
		if err != nil {
			return DelegateConfig{}, err
		}
		cfg := DelegateConfig{}
		known := map[string]bool{"all": true, "build": true, "env": true, "wrap": true, "packages": true}
		for key, raw := range table {
			if !known[key] {
				return DelegateConfig{}, diagAt("unknown_key", path, "unknown delegate key %q", key)
			}
			b, err := expectBool(path, "delegate."+key, raw)
			if err != nil {
				return DelegateConfig{}, err
			}
			switch key {
			case "all":
				cfg.All = b
			case "build":
				cfg.Build = b
			case "env":
				cfg.Env = b
			case "wrap":
				cfg.Wrap = b
			case "packages":
				cfg.Packages = b
			}
		}
		return cfg, nil
	}
}

func parseSourcePackage(path string, value any) (SourcePackageConfig, error) {
	table, err := expectMap(path, "source_package", value)
	if err != nil {
		return SourcePackageConfig{}, err
	}
	cfg := SourcePackageConfig{}
	known := map[string]bool{"name": true, "enabled": true}
	for key, raw := range table {
		if !known[key] {
			return SourcePackageConfig{}, diagAt("unknown_key", path, "unknown source_package key %q", key)
		}
		switch key {
		case "name":
			name, err := expectString(path, "source_package.name", raw)
			if err != nil {
				return SourcePackageConfig{}, err
			}
			if name == "" {
				return SourcePackageConfig{}, diagAt("invalid_value", path, "source_package.name must not be empty")
			}
			cfg.Name = name
		case "enabled":
			b, err := expectBool(path, "source_package.enabled", raw)
			if err != nil {
				return SourcePackageConfig{}, err
			}
			cfg.Enabled = &b
		}
	}
	return cfg, nil
}

func parsePackageMeta(path string, value any) (PackageMeta, error) {
	table, err := expectMap(path, "package", value)
	if err != nil {
		return PackageMeta{}, err
	}
	known := map[string]bool{"name": true, "version": true, "architecture": true, "description": true, "license": true, "license_class": true, "homepage": true, "default_root": true, "special_system_package": true, "alternate_upgrade": true}
	for key := range table {
		if !known[key] {
			return PackageMeta{}, diagAt("unknown_key", path, "unknown package metadata key %q", key)
		}
	}
	meta := PackageMeta{}
	for key, raw := range table {
		// The one boolean in this table; everything else is a string.
		if key == "special_system_package" {
			b, err := expectBool(path, "package.special_system_package", raw)
			if err != nil {
				return PackageMeta{}, err
			}
			meta.SpecialSystemPackage = b
			continue
		}
		// The one table: alternate_upgrade carries only a message.
		if key == "alternate_upgrade" {
			alt, err := parseAlternateUpgrade(path, raw)
			if err != nil {
				return PackageMeta{}, err
			}
			meta.AlternateUpgrade = alt
			continue
		}
		s, err := expectString(path, "package."+key, raw)
		if err != nil {
			return PackageMeta{}, err
		}
		switch key {
		case "name":
			meta.Name = s
		case "version":
			meta.Version = s
		case "architecture":
			meta.Architecture = s
		case "description":
			meta.Description = s
		case "license":
			meta.License = s
		case "license_class":
			if !validLicenseClass(s) {
				return PackageMeta{}, diagAt("invalid_license_class", path,
					"package.license_class: %q is not one of unknown, free, firmware, proprietary", s)
			}
			meta.LicenseClass = s
		case "homepage":
			meta.Homepage = s
		case "default_root":
			if err := validRootRef(s); err != nil {
				return PackageMeta{}, diagAt("invalid_root", path, "package.default_root: %v", err)
			}
			meta.DefaultRoot = s
		}
	}
	return meta, nil
}

// parseAlternateUpgrade parses the [package] alternate_upgrade table
// (§5.18): a table whose only key is message, which mirrors the
// consumer's rule — non-empty UTF-8 of at most 1024 bytes, newlines
// permitted and no other control character — so a recipe author sees
// a bad message at plan time, not at install time.
func parseAlternateUpgrade(path string, value any) (*AlternateUpgradeMeta, error) {
	table, err := expectMap(path, "package.alternate_upgrade", value)
	if err != nil {
		return nil, err
	}
	for key := range table {
		if key != "message" {
			return nil, diagAt("unknown_key", path, "unknown package.alternate_upgrade key %q", key)
		}
	}
	raw, ok := table["message"]
	if !ok {
		return nil, diagAt("missing_package_field", path,
			"package.alternate_upgrade requires message")
	}
	msg, err := expectString(path, "package.alternate_upgrade.message", raw)
	if err != nil {
		return nil, err
	}
	if err := validAlternateUpgradeMessage(msg); err != nil {
		return nil, diagAt("invalid_value", path, "package.alternate_upgrade.message: %v", err)
	}
	return &AlternateUpgradeMeta{Message: msg}, nil
}

// validAlternateUpgradeMessage checks an alternate_upgrade message
// against §5.18.
func validAlternateUpgradeMessage(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(s) > 1024 {
		return fmt.Errorf("is %d bytes, the limit is 1024", len(s))
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("is not valid UTF-8")
	}
	for i, r := range s {
		if r != '\n' && unicode.IsControl(r) {
			return fmt.Errorf("contains the control character %#02x at offset %d", r, i)
		}
	}
	return nil
}

// validLicenseClass checks a license_class value against the closed set
// of PSPU book 5 §3.3.6. Mirrors the consumer's check so a recipe author
// sees the typo at plan time, not at install time.
func validLicenseClass(s string) bool {
	switch s {
	case "unknown", "free", "firmware", "proprietary":
		return true
	}
	return false
}

// validRootRef checks a root reference against the §3.3.6 grammar: dotted
// segments, each [a-z0-9][a-z0-9_-]*, and never a filesystem path (the
// presence of '/' marks a path). It mirrors the consumer's check so a
// recipe-author sees a placement typo at build time, not install time.
func validRootRef(s string) error {
	if s == "" {
		return fmt.Errorf("root reference must not be empty")
	}
	if strings.ContainsRune(s, '/') {
		return fmt.Errorf("%q must be a named reference, not a filesystem path", s)
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return fmt.Errorf("%q has an empty segment", s)
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case (c == '-' || c == '_') && i > 0:
			default:
				return fmt.Errorf("%q has an invalid segment %q", s, seg)
			}
		}
	}
	return nil
}

// parseDependencyMap parses a dependencies or optional_dependencies table
// where each entry is either a bare constraint string (the common short
// form → the depender's own root) or a table { constraint, root } that
// places the dependency in a named root (§4.1.1, DESIGN-named-roots.md).
// It returns parallel maps: constraints (name → constraint) and roots
// (name → placement root, only for entries that set one). The table form
// is the only way to place a dependency — there is no IN-string sugar.
func parseDependencyMap(path, key string, value any) (constraints, roots map[string]string, err error) {
	table, err := expectMap(path, key, value)
	if err != nil {
		return nil, nil, err
	}
	constraints = map[string]string{}
	roots = map[string]string{}
	for k, raw := range table {
		switch v := raw.(type) {
		case string:
			constraints[k] = v
		default:
			sub, err := expectMap(path, key+"."+k, raw)
			if err != nil {
				return nil, nil, err
			}
			knownSub := map[string]bool{"constraint": true, "root": true}
			for sk := range sub {
				if !knownSub[sk] {
					return nil, nil, diagAt("unknown_key", path,
						"unknown %s.%s key %q (a dependency table takes constraint and root)", key, k, sk)
				}
			}
			if c, ok := sub["constraint"]; ok {
				cs, ok := c.(string)
				if !ok {
					return nil, nil, diagAt("invalid_type", path, "%s.%s.constraint must be a string", key, k)
				}
				constraints[k] = cs
			} else {
				constraints[k] = "" // a table without a constraint matches any version
			}
			if r, ok := sub["root"]; ok {
				rs, ok := r.(string)
				if !ok {
					return nil, nil, diagAt("invalid_type", path, "%s.%s.root must be a string", key, k)
				}
				if err := validRootRef(rs); err != nil {
					return nil, nil, diagAt("invalid_root", path, "%s.%s.root: %v", key, k, err)
				}
				roots[k] = rs
			}
		}
	}
	return constraints, roots, nil
}

func parsePackageFiles(path string, value any) (map[string]PackageFileEntry, error) {
	table, err := expectMap(path, "files", value)
	if err != nil {
		return nil, err
	}
	out := map[string]PackageFileEntry{}
	for src, raw := range table {
		entry := PackageFileEntry{Source: src}
		switch v := raw.(type) {
		case string:
			entry.Path = v
		default:
			sub, err := expectMap(path, "files."+src, raw)
			if err != nil {
				return nil, err
			}
			known := map[string]bool{"path": true, "override": true}
			for key := range sub {
				if !known[key] {
					return nil, diagAt("unknown_key", path, "unknown files.%q key %q", src, key)
				}
			}
			p, err := requiredString(path, "files."+src+".path", sub)
			if err != nil {
				return nil, err
			}
			entry.Path = p
			if v, ok := sub["override"]; ok {
				entry.Override, err = expectBool(path, "files."+src+".override", v)
				if err != nil {
					return nil, err
				}
			}
		}
		if entry.Path == "" {
			return nil, diagAt("missing_key", path, "file mapping %q has empty destination", src)
		}
		out[src] = entry
	}
	return out, nil
}

func parsePackageSymlinks(path string, value any) (map[string]PackageSymlinkEntry, error) {
	table, err := expectMap(path, "symlinks", value)
	if err != nil {
		return nil, err
	}
	out := map[string]PackageSymlinkEntry{}
	for dest, raw := range table {
		entry := PackageSymlinkEntry{}
		switch v := raw.(type) {
		case string:
			entry.Target = v
		default:
			sub, err := expectMap(path, "symlinks."+dest, raw)
			if err != nil {
				return nil, err
			}
			known := map[string]bool{"target": true, "override": true}
			for key := range sub {
				if !known[key] {
					return nil, diagAt("unknown_key", path, "unknown symlinks.%q key %q", dest, key)
				}
			}
			entry.Target, err = requiredString(path, "symlinks."+dest+".target", sub)
			if err != nil {
				return nil, err
			}
			if v, ok := sub["override"]; ok {
				entry.Override, err = expectBool(path, "symlinks."+dest+".override", v)
				if err != nil {
					return nil, err
				}
			}
		}
		if entry.Target == "" {
			return nil, diagAt("missing_key", path, "symlink %q has empty target", dest)
		}
		out[dest] = entry
	}
	return out, nil
}

func parseMultipack(path string, value any) (MultipackConfig, error) {
	table, err := expectMap(path, "multipack", value)
	if err != nil {
		return MultipackConfig{}, err
	}
	cfg := MultipackConfig{}
	known := map[string]bool{"enum": true}
	for key := range table {
		if !known[key] {
			return MultipackConfig{}, diagAt("unknown_key", path, "unknown multipack key %q", key)
		}
	}
	if v, ok := table["enum"]; ok {
		switch raw := v.(type) {
		case []any:
			cfg.Enum, err = parseMultipackEnumValues(path, raw)
			if err != nil {
				return MultipackConfig{}, err
			}
		default:
			enum, err := expectMap(path, "multipack.enum", v)
			if err != nil {
				return MultipackConfig{}, err
			}
			filesRaw, ok := enum["files"]
			if !ok {
				return MultipackConfig{}, diagAt("missing_key", path, "multipack.enum requires files")
			}
			files, err := expectMap(path, "multipack.enum.files", filesRaw)
			if err != nil {
				return MultipackConfig{}, err
			}
			for key := range files {
				if key != "path" && key != "regex" {
					return MultipackConfig{}, diagAt("unknown_key", path, "unknown multipack.enum.files key %q", key)
				}
			}
			cfg.EnumFiles.Path, err = requiredString(path, "multipack.enum.files.path", files)
			if err != nil {
				return MultipackConfig{}, err
			}
			cfg.EnumFiles.Regex, err = requiredString(path, "multipack.enum.files.regex", files)
			if err != nil {
				return MultipackConfig{}, err
			}
		}
	}
	return cfg, nil
}

func parseMultipackEnumValues(path string, values []any) ([]string, error) {
	if len(values) == 0 {
		return nil, diagAt("empty_multipack", path, "multipack.enum must not be empty")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for idx, raw := range values {
		value, ok := raw.(string)
		if !ok {
			return nil, diagAt("invalid_type", path, "multipack.enum[%d] must be a string", idx)
		}
		if err := validateSelector("multipack value", value); err != nil {
			return nil, diagAt("invalid_selector", path, "%s", err.Error())
		}
		if seen[value] {
			return nil, diagAt("duplicate_multipack", path, "duplicate multipack value %q", value)
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

func parsePublish(path string, value any) (PublishConfig, error) {
	table, err := expectMap(path, "publish", value)
	if err != nil {
		return PublishConfig{}, err
	}
	cfg := PublishConfig{Defined: true}
	for key, raw := range table {
		switch key {
		case "localdir":
			items, err := expectArray(path, "publish.localdir", raw)
			if err != nil {
				return PublishConfig{}, err
			}
			for idx, item := range items {
				sub, err := expectMap(path, fmt.Sprintf("publish.localdir[%d]", idx), item)
				if err != nil {
					return PublishConfig{}, err
				}
				for k := range sub {
					if k != "path" && k != "overwrite" {
						return PublishConfig{}, diagAt("unknown_key", path, "unknown publish.localdir key %q", k)
					}
				}
				p, err := requiredString(path, "publish.localdir.path", sub)
				if err != nil {
					return PublishConfig{}, err
				}
				target := LocalDirPublish{Path: p}
				if v, ok := sub["overwrite"]; ok {
					b, err := expectBool(path, "publish.localdir.overwrite", v)
					if err != nil {
						return PublishConfig{}, err
					}
					target.Overwrite = &b
				}
				cfg.LocalDir = append(cfg.LocalDir, target)
			}
		case "peipkg":
			sub, err := expectMap(path, "publish.peipkg", raw)
			if err != nil {
				return PublishConfig{}, err
			}
			for k := range sub {
				if k != "path" && k != "name" && k != "signing_key" {
					return PublishConfig{}, diagAt("unknown_key", path, "unknown publish.peipkg key %q", k)
				}
			}
			p, err := requiredString(path, "publish.peipkg.path", sub)
			if err != nil {
				return PublishConfig{}, err
			}
			key, err := requiredString(path, "publish.peipkg.signing_key", sub)
			if err != nil {
				return PublishConfig{}, err
			}
			target := PeipkgPublish{Path: p, SigningKey: key}
			if v, ok := sub["name"]; ok {
				target.Name, err = expectString(path, "publish.peipkg.name", v)
				if err != nil {
					return PublishConfig{}, err
				}
			}
			cfg.Peipkg = &target
		default:
			return PublishConfig{}, diagAt("unknown_key", path, "unknown publish target %q", key)
		}
	}
	return cfg, nil
}

func parseWrap(path, key string, value any) (ShellCommand, error) {
	table, err := expectMap(path, key, value)
	if err != nil {
		return ShellCommand{}, err
	}
	for k := range table {
		if k != "command" {
			return ShellCommand{}, diagAt("unknown_key", path, "unknown %s key %q", key, k)
		}
	}
	raw, ok := table["command"]
	if !ok {
		return ShellCommand{}, nil
	}
	cmd, err := parseCommand(path, key+".command", raw)
	if err != nil {
		return ShellCommand{}, err
	}
	if err := validateWrapCommand(path, key+".command", cmd); err != nil {
		return ShellCommand{}, err
	}
	return cmd, nil
}

func parseCommand(path, key string, value any) (ShellCommand, error) {
	switch v := value.(type) {
	case string:
		return ShellCommand{Shell: v}, nil
	case []any:
		args := make([]string, 0, len(v))
		for idx, item := range v {
			s, ok := item.(string)
			if !ok {
				return ShellCommand{}, diagAt("invalid_type", path, "%s[%d] must be a string", key, idx)
			}
			args = append(args, s)
		}
		if len(args) == 0 {
			return ShellCommand{}, diagAt("invalid_type", path, "%s array must not be empty", key)
		}
		return ShellCommand{Args: args}, nil
	default:
		return ShellCommand{}, diagAt("invalid_type", path, "%s must be a string or array of strings", key)
	}
}

func validateWrapCommand(path, key string, cmd ShellCommand) error {
	if cmd.Shell != "" {
		if strings.Count(cmd.Shell, "{{command}}") != 1 {
			return diagAt("invalid_wrap", path, "%s must contain exactly one {{command}} placeholder", key)
		}
		return nil
	}
	count := 0
	for idx, arg := range cmd.Args {
		if strings.Contains(arg, "{{command}}") && arg != "{{command}}" {
			return diagAt("invalid_wrap", path, "%s[%d] must use {{command}} as a complete argument", key, idx)
		}
		if arg == "{{command}}" {
			if idx == 0 {
				return diagAt("invalid_wrap", path, "%s[0] is the program and cannot be {{command}}", key)
			}
			count++
		}
	}
	if count != 1 {
		return diagAt("invalid_wrap", path, "%s must contain exactly one {{command}} placeholder", key)
	}
	return nil
}

func parseStringMap(path, key string, value any) (map[string]string, error) {
	table, err := expectMap(path, key, value)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, raw := range table {
		s, ok := raw.(string)
		if !ok {
			return nil, diagAt("invalid_type", path, "%s.%s must be a string", key, k)
		}
		out[k] = s
	}
	return out, nil
}

func parseEnvVars(path, key string, value any, md toml.MetaData) ([]EnvVar, error) {
	table, err := expectMap(path, key, value)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []EnvVar
	for _, metaKey := range md.Keys() {
		if len(metaKey) != 2 || metaKey[0] != key {
			continue
		}
		name := metaKey[1]
		if !envNameRE.MatchString(name) {
			return nil, diagAt("invalid_env", path, "%s.%s is not a valid environment variable name", key, name)
		}
		raw, ok := table[name]
		if !ok {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return nil, diagAt("invalid_type", path, "%s.%s must be a string", key, name)
		}
		seen[name] = true
		out = append(out, EnvVar{Name: name, Value: value})
	}
	for _, name := range sortedKeys(table) {
		if seen[name] {
			continue
		}
		if !envNameRE.MatchString(name) {
			return nil, diagAt("invalid_env", path, "%s.%s is not a valid environment variable name", key, name)
		}
		value, ok := table[name].(string)
		if !ok {
			return nil, diagAt("invalid_type", path, "%s.%s must be a string", key, name)
		}
		out = append(out, EnvVar{Name: name, Value: value})
	}
	return out, nil
}

func requiredString(path, key string, table map[string]any) (string, error) {
	raw, ok := table[strings.TrimPrefix(key, strings.Split(key, ".")[0]+".")]
	if !ok {
		parts := strings.Split(key, ".")
		raw, ok = table[parts[len(parts)-1]]
	}
	if !ok {
		return "", diagAt("missing_key", path, "%s is required", key)
	}
	return expectString(path, key, raw)
}

func expectString(path, key string, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", diagAt("invalid_type", path, "%s must be a string", key)
	}
	return s, nil
}

func expectBool(path, key string, value any) (bool, error) {
	b, ok := value.(bool)
	if !ok {
		return false, diagAt("invalid_type", path, "%s must be a bool", key)
	}
	return b, nil
}

func expectNonnegativeInt(path, key string, value any) (int, error) {
	n, ok := value.(int64)
	if !ok {
		return 0, diagAt("invalid_type", path, "%s must be an integer", key)
	}
	if n < 0 || n > int64(^uint(0)>>1) {
		return 0, diagAt("invalid_value", path, "%s must be a non-negative integer", key)
	}
	return int(n), nil
}

func expectStringSlice(path, key string, value any) ([]string, error) {
	items, err := expectArray(path, key, value)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for idx, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, diagAt("invalid_type", path, "%s[%d] must be a string", key, idx)
		}
		out = append(out, s)
	}
	return out, nil
}

func expectArray(path, key string, value any) ([]any, error) {
	switch v := value.(type) {
	case []any:
		return v, nil
	case []map[string]any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return out, nil
	default:
		return nil, diagAt("invalid_type", path, "%s must be an array", key)
	}
}

func expectMap(path, key string, value any) (map[string]any, error) {
	switch v := value.(type) {
	case map[string]any:
		return v, nil
	default:
		return nil, diagAt("invalid_type", path, "%s must be a table", key)
	}
}

func diagAt(code, path, msg string, args ...any) error {
	d := Diagnostic{Code: code, Path: path}
	if len(args) > 0 {
		d.Message = fmt.Sprintf(msg, args...)
	} else {
		d.Message = msg
	}
	return d
}
