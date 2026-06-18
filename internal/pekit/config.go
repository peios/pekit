package pekit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
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
	Name     string
	Kind     Command
	Command  ShellCommand
	Needs    []string
	ClearOut bool
	Owner    string
	Path     string
}

type RecipeConfig struct {
	Root     string
	Path     string
	OutDir   string
	Env      map[string]string
	Wrap     ShellCommand
	Targets  map[Command]map[string]TargetConfig
	Source   SourceConfig
	Delegate DelegateConfig
}

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

type SourceConfig struct {
	Git   GitSourceConfig
	URL   URLSourceConfig
	Local LocalSourceConfig
}

func (s SourceConfig) HasReproducible() bool { return s.Git.URL != "" || s.URL.URL != "" }
func (s SourceConfig) HasExternal() bool     { return s.HasReproducible() || s.Local.Path != "" }

type GitSourceConfig struct {
	URL      string
	Ref      string
	Versions string
	TagRegex string
}

type URLSourceConfig struct {
	URL               string
	Extract           bool
	Root              string
	Versions          string
	FileRegex         string
	Checksum          string
	ChecksumByVersion map[string]string
}

type LocalSourceConfig struct {
	Path         string
	ResolvedPath string
}

type WorkspaceConfig struct {
	Root    string
	Path    string
	Include []string
	Exclude []string
	Env     map[string]string
	Wrap    ShellCommand
}

type EnvFile struct {
	Path string
	Env  map[string]string
	Wrap ShellCommand
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

type PackageMeta struct {
	Name         string
	Version      string
	Architecture string
	Description  string
	License      string
	Homepage     string

	Dependencies         map[string]string
	OptionalDependencies map[string]string
	Conflicts            map[string]string
	Provides             map[string]string
	Replaces             map[string]string
	SideEffects          []string
	SDOverrides          map[string]string
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
	LocalDir []LocalDirPublish
}

type LocalDirPublish struct {
	Path      string
	Overwrite *bool
}

func LoadRecipe(path string) (RecipeConfig, error) {
	root := filepath.Dir(path)
	raw, err := loadTOML(path)
	if err != nil {
		return RecipeConfig{}, err
	}
	cfg := RecipeConfig{
		Root:    root,
		Path:    path,
		OutDir:  "out",
		Env:     map[string]string{},
		Targets: map[Command]map[string]TargetConfig{},
	}
	known := map[string]bool{
		"out_dir": true, "env": true, "wrap": true, "source": true, "delegate": true,
		"build": true, "test": true, "install": true, "clean": true,
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
	if v, ok := raw["env"]; ok {
		cfg.Env, err = parseEnvMap(path, "env", v)
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
	for _, cmd := range []Command{CommandBuild, CommandTest, CommandInstall, CommandClean} {
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

func LoadWorkspace(path string) (WorkspaceConfig, error) {
	root := filepath.Dir(path)
	raw, err := loadTOML(path)
	if err != nil {
		return WorkspaceConfig{}, err
	}
	cfg := WorkspaceConfig{Root: root, Path: path, Env: map[string]string{}}
	known := map[string]bool{"include": true, "exclude": true, "env": true, "wrap": true}
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
		cfg.Env, err = parseEnvMap(path, "env", v)
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
	return cfg, nil
}

func LoadEnvFile(path string, missingOK bool) (EnvFile, error) {
	if !fileExists(path) {
		if missingOK {
			return EnvFile{Path: path, Env: map[string]string{}}, nil
		}
		return EnvFile{}, diagAt("missing_env_file", path, "env file does not exist")
	}
	raw, err := loadTOML(path)
	if err != nil {
		return EnvFile{}, err
	}
	env := EnvFile{Path: path, Env: map[string]string{}}
	known := map[string]bool{"env": true, "wrap": true}
	for key := range raw {
		if !known[key] {
			return EnvFile{}, diagAt("unknown_key", path, "unknown env-file key %q", key)
		}
	}
	if _, hasEnv := raw["env"]; !hasEnv {
		if _, hasWrap := raw["wrap"]; !hasWrap {
			return EnvFile{}, diagAt("missing_key", path, "env file requires [env], [wrap], or both")
		}
	}
	if v, ok := raw["env"]; ok {
		env.Env, err = parseEnvMap(path, "env", v)
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
		cfg.Package.Dependencies, err = parseStringMap(path, "dependencies", v)
		if err != nil {
			return PackageConfig{}, err
		}
	}
	if v, ok := raw["optional_dependencies"]; ok {
		cfg.Package.OptionalDependencies, err = parseStringMap(path, "optional_dependencies", v)
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

func loadTOML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, wrapDiag("read_file", path, err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, wrapDiag("parse_toml", path, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
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
			if _, ok := v.(map[string]any); ok && key != "command" {
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

func parseTarget(path string, kind Command, name string, table map[string]any) (TargetConfig, error) {
	known := map[string]bool{"command": true, "needs": true, "clear_out": true}
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
	if v, ok := table["clear_out"]; ok {
		t.ClearOut, err = expectBool(path, string(kind)+"."+name+".clear_out", v)
		if err != nil {
			return TargetConfig{}, err
		}
	}
	return t, nil
}

func parseSource(path, root string, value any) (SourceConfig, error) {
	table, err := expectMap(path, "source", value)
	if err != nil {
		return SourceConfig{}, err
	}
	cfg := SourceConfig{}
	known := map[string]bool{"git": true, "url": true, "local": true}
	for key := range table {
		if !known[key] {
			return SourceConfig{}, diagAt("unknown_key", path, "unknown source key %q; v2 uses [source.git], [source.url], and [source.local]", key)
		}
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
	return cfg, nil
}

func parseGitSource(path string, table map[string]any) (GitSourceConfig, error) {
	known := map[string]bool{"url": true, "ref": true, "versions": true, "tag_regex": true}
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
	return cfg, nil
}

func parseURLSource(path string, table map[string]any) (URLSourceConfig, error) {
	known := map[string]bool{"url": true, "extract": true, "root": true, "versions": true, "file_regex": true, "checksum": true}
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

func parsePackageMeta(path string, value any) (PackageMeta, error) {
	table, err := expectMap(path, "package", value)
	if err != nil {
		return PackageMeta{}, err
	}
	known := map[string]bool{"name": true, "version": true, "architecture": true, "description": true, "license": true, "homepage": true}
	for key := range table {
		if !known[key] {
			return PackageMeta{}, diagAt("unknown_key", path, "unknown package metadata key %q", key)
		}
	}
	meta := PackageMeta{}
	for key, raw := range table {
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
		case "homepage":
			meta.Homepage = s
		}
	}
	return meta, nil
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
	cfg := PublishConfig{}
	for key, raw := range table {
		if key != "localdir" {
			return PublishConfig{}, diagAt("unknown_key", path, "unknown publish target %q", key)
		}
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

func parseEnvMap(path, key string, value any) (map[string]string, error) {
	out, err := parseStringMap(path, key, value)
	if err != nil {
		return nil, err
	}
	for name := range out {
		if !envNameRE.MatchString(name) {
			return nil, diagAt("invalid_env", path, "%s.%s is not a valid environment variable name", key, name)
		}
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
