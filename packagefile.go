package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// PackageFile is a parsed package.pekit.toml: one file, one package.
// Fields grow only when a format needs them; the peipkg format needed
// most of the PSD-009 manifest surface.
type PackageFile struct {
	Format string
	Files  []FileMapping // sorted by Dest

	// Exclude are [files].exclude source patterns: staged files matching any
	// of them are dropped after a glob expands but before packing, so a broad
	// "usr/bin/**" can ship everything except a handful of named tools. Same
	// target:path syntax as [files] keys; globs allowed. Matched against the
	// source path (stage-relative), not the rebased dest.
	Exclude []SourceRef

	// Builds names build targets to run before packaging, for literal-
	// path sources whose producer cannot be derived. ADDITIVE: unioned
	// with the targets derived from stage references, never replacing
	// them (an override mode could silently skip a referenced build).
	Builds []string

	// [package] identity. Name is an optional override (default: the
	// project dir name); the rest is required or optional per-format.
	Name         string
	Version      string
	Architecture string
	Description  string
	License      string
	Homepage     string

	Dependencies         []Dependency
	OptionalDependencies []Dependency
	Conflicts            []Dependency
	Provides             []Provides
	Replaces             []Replaces

	// SideEffects order is semantic (PSD-009 §4.3.4), so it is an
	// array, not a name-keyed table.
	SideEffects []string

	// SDOverrides hold SDDL (compiled to binary SDs at pack time);
	// sorted by path.
	SDOverrides []SDOverride

	// Publish are the destinations a built package ships to. Operational,
	// not manifest data (like Builds) — never projected into the .peipkg.
	// Typically supplied by the workspace-root package.pekit.toml and
	// inherited; sorted by (Type, Path).
	Publish []PublishTarget
}

// PublishTarget is one [[publish.<type>]] entry: where a built package is
// shipped. Type names the mechanism; the remaining fields are
// type-specific (only localdir's Path today).
type PublishTarget struct {
	Type string // "localdir"
	Path string // localdir: destination dir, relative to the workspace root
}

// Dependency is one [dependencies]/[optionalDependencies]/[conflicts]
// entry. Constraint "" means any version (written "*" in TOML).
type Dependency struct {
	Name       string
	Constraint string
	Arch       string
}

// Provides is one [provides] entry: a virtual capability satisfied.
type Provides struct {
	Name    string
	Version string
}

// Replaces is one [replaces] entry: a package superseded on upgrade.
type Replaces struct {
	Name       string
	Constraint string
}

// SDOverride is one [sdOverrides] entry: an explicit security
// descriptor for one packaged path, in SDDL.
type SDOverride struct {
	Path string
	SDDL string
}

// manifestExtras names every set field that exists for manifest-bearing
// formats, so formats without a manifest (tar) can reject them loudly
// instead of dropping them silently.
func (pf *PackageFile) manifestExtras() []string {
	var extras []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"version", pf.Version != ""},
		{"architecture", pf.Architecture != ""},
		{"description", pf.Description != ""},
		{"license", pf.License != ""},
		{"homepage", pf.Homepage != ""},
		{"dependencies", len(pf.Dependencies) > 0},
		{"optionalDependencies", len(pf.OptionalDependencies) > 0},
		{"conflicts", len(pf.Conflicts) > 0},
		{"provides", len(pf.Provides) > 0},
		{"replaces", len(pf.Replaces) > 0},
		{"sideEffects", len(pf.SideEffects) > 0},
		{"sdOverrides", len(pf.SDOverrides) > 0},
	} {
		if f.set {
			extras = append(extras, f.name)
		}
	}
	return extras
}

// FileMapping maps one build output to its install path.
type FileMapping struct {
	Source SourceRef
	Dest   string // image-relative, cleaned, no leading slash
}

// SourceRef is a [files] source. "target:path" references path inside
// build target's stage (":path" is sugar for "main:path"); a plain path
// is project-relative, for build systems that cannot redirect output
// into the stage.
type SourceRef struct {
	Target string // build target whose stage holds Path; empty = literal
	Path   string
}

func (s SourceRef) String() string {
	if s.Target == "" {
		return s.Path
	}
	return s.Target + ":" + s.Path
}

// decodePackageFile reads, templates, and TOML-decodes a package.pekit.toml
// into a raw table without parsing or validating it, so it can be merged
// with another (a source's, in delegate mode) before parsing. Returns
// (nil, false, nil) when the file is absent; real read/render/decode
// errors propagate.
func decodePackageFile(path string, ver *Version) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// Defer {{multipack}}: a [multipack] recipe binds it once per enum value
	// after this pass (see multipack.go). Leaving it intact here means a
	// version-less multipack recipe renders without --version.
	rendered, err := renderTemplate(string(data), ver, "multipack")
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	var raw map[string]any
	if _, err := toml.Decode(rendered, &raw); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return raw, true, nil
}

// mergePackageRaw overlays a recipe's package.pekit.toml table onto the
// source's, implementing per-section delegation: [package] merges
// field-by-field (a recipe field wins, the source fills the rest), while
// every other top-level key (format, files, dependencies, ...) is
// whole-unit — present in the recipe replaces the source's, absent
// inherits it. Either side may be nil.
func mergePackageRaw(recipe, source map[string]any) map[string]any {
	if recipe == nil {
		return source
	}
	if source == nil {
		return recipe
	}
	out := make(map[string]any, len(source))
	for k, v := range source {
		out[k] = v
	}
	for k, v := range recipe {
		if k == "package" {
			out[k] = mergePackageTable(source[k], v)
			continue
		}
		out[k] = v
	}
	return out
}

// mergePackageTable field-merges two [package] tables (recipe over
// source). If either operand is not a table, the recipe's value stands
// whole so parsing reports the type error against it rather than silently
// blending mismatched shapes.
func mergePackageTable(source, recipe any) any {
	srcT, sok := source.(map[string]any)
	recT, rok := recipe.(map[string]any)
	if !sok || !rok {
		return recipe
	}
	out := make(map[string]any, len(srcT)+len(recT))
	for k, v := range srcT {
		out[k] = v
	}
	for k, v := range recT {
		out[k] = v
	}
	return out
}

// ParsePackageFile parses package.pekit.toml source.
func ParsePackageFile(src string) (*PackageFile, error) {
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}
	return parsePackageRaw(raw)
}

// parsePackageRaw turns an already-decoded package.pekit.toml table into a
// PackageFile. Split from ParsePackageFile so a merged table (recipe
// overlaid on source) parses through exactly the same path and validation.
func parsePackageRaw(raw map[string]any) (*PackageFile, error) {
	pf := &PackageFile{}

	for _, key := range sortedKeys(raw) {
		switch key {
		case "format":
			s, err := stringValue("root", key, raw[key])
			if err != nil {
				return nil, err
			}
			pf.Format = s
		case "builds":
			vals, ok := raw[key].([]any)
			if !ok {
				return nil, fmt.Errorf("builds must be an array of build target names")
			}
			if len(vals) == 0 {
				return nil, fmt.Errorf("builds = [] does nothing (stage-referenced targets always build); omit it")
			}
			for _, v := range vals {
				s, ok := v.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("builds must be an array of non-empty build target names")
				}
				pf.Builds = append(pf.Builds, s)
			}
		case "package":
			table, ok := raw[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[package] must be a table")
			}
			if err := pf.parseIdentity(table); err != nil {
				return nil, err
			}
		case "dependencies", "optionalDependencies", "conflicts":
			deps, err := parseDeps(key, raw[key])
			if err != nil {
				return nil, err
			}
			switch key {
			case "dependencies":
				pf.Dependencies = deps
			case "optionalDependencies":
				pf.OptionalDependencies = deps
			case "conflicts":
				pf.Conflicts = deps
			}
		case "provides":
			entries, err := parseNameValueTable(key, raw[key])
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				pf.Provides = append(pf.Provides, Provides{Name: e[0], Version: e[1]})
			}
		case "replaces":
			entries, err := parseNameValueTable(key, raw[key])
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				pf.Replaces = append(pf.Replaces, Replaces{Name: e[0], Constraint: e[1]})
			}
		case "sdOverrides":
			table, ok := raw[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[sdOverrides] must be a table")
			}
			overrides, err := parseSDOverrides(table)
			if err != nil {
				return nil, err
			}
			pf.SDOverrides = overrides
		case "files":
			table, ok := raw[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[files] must be a table")
			}
			files, excludes, err := parseFiles(table)
			if err != nil {
				return nil, err
			}
			pf.Files = files
			pf.Exclude = excludes
		case "publish":
			table, ok := raw[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[publish] must be a table of typed targets ([[publish.<type>]])")
			}
			targets, err := parsePublish(table)
			if err != nil {
				return nil, err
			}
			pf.Publish = targets
		default:
			return nil, fmt.Errorf("unknown key %q", key)
		}
	}

	if pf.Format == "" {
		return nil, fmt.Errorf("missing required key %q", "format")
	}
	if len(pf.Files) == 0 {
		return nil, fmt.Errorf("[files] must map at least one file")
	}
	return pf, nil
}

func (pf *PackageFile) parseIdentity(table map[string]any) error {
	for _, key := range sortedKeys(table) {
		if key == "sideEffects" {
			vals, ok := table[key].([]any)
			if !ok {
				return fmt.Errorf("[package]: sideEffects must be an array of strings")
			}
			for _, v := range vals {
				s, ok := v.(string)
				if !ok || s == "" {
					return fmt.Errorf("[package]: sideEffects must be an array of non-empty strings")
				}
				pf.SideEffects = append(pf.SideEffects, s)
			}
			continue
		}
		dst, ok := map[string]*string{
			"name":         &pf.Name,
			"version":      &pf.Version,
			"architecture": &pf.Architecture,
			"description":  &pf.Description,
			"license":      &pf.License,
			"homepage":     &pf.Homepage,
		}[key]
		if !ok {
			return fmt.Errorf("[package]: unknown key %q", key)
		}
		s, err := stringValue("package", key, table[key])
		if err != nil {
			return err
		}
		*dst = s
	}
	return nil
}

// parseDeps parses a dependency table. Values are either a constraint
// string ("*" = any version) or an inline table {constraint, arch}.
func parseDeps(section string, raw any) ([]Dependency, error) {
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("[%s] must be a table", section)
	}
	var deps []Dependency
	for _, name := range sortedKeys(table) {
		dep := Dependency{Name: name}
		switch v := table[name].(type) {
		case string:
			if v == "" {
				return nil, fmt.Errorf("[%s]: %s: use %q for any version, not an empty string", section, name, "*")
			}
			dep.Constraint = v
		case map[string]any:
			for _, k := range sortedKeys(v) {
				switch k {
				case "constraint", "arch":
					s, err := stringValue(section+"."+name, k, v[k])
					if err != nil {
						return nil, err
					}
					if k == "constraint" {
						dep.Constraint = s
					} else {
						dep.Arch = s
					}
				default:
					return nil, fmt.Errorf("[%s.%s]: unknown key %q", section, name, k)
				}
			}
		default:
			return nil, fmt.Errorf("[%s]: %s must be a constraint string or {constraint, arch} table", section, name)
		}
		if dep.Constraint == "*" {
			dep.Constraint = "" // pack's any-version spelling
		}
		deps = append(deps, dep)
	}
	return deps, nil
}

// parseNameValueTable parses a flat string->string table ([provides],
// [replaces]) into sorted (name, value) pairs, translating "*" to "".
func parseNameValueTable(section string, raw any) ([][2]string, error) {
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("[%s] must be a table", section)
	}
	var entries [][2]string
	for _, name := range sortedKeys(table) {
		s, err := stringValue(section, name, table[name])
		if err != nil {
			return nil, err
		}
		if s == "*" {
			s = ""
		}
		entries = append(entries, [2]string{name, s})
	}
	return entries, nil
}

func parseSDOverrides(table map[string]any) ([]SDOverride, error) {
	var overrides []SDOverride
	for _, key := range sortedKeys(table) {
		cleaned, err := cleanDest(key)
		if err != nil {
			return nil, fmt.Errorf("[sdOverrides]: %w", err)
		}
		s, err := stringValue("sdOverrides", key, table[key])
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, SDOverride{Path: cleaned, SDDL: s})
	}
	return overrides, nil
}

// parsePublish parses the [publish] table, whose keys are target types
// ([[publish.localdir]] -> "localdir") each holding an array of target
// tables. Targets are returned sorted by (type, path) for determinism.
func parsePublish(table map[string]any) ([]PublishTarget, error) {
	var targets []PublishTarget
	for _, typ := range sortedKeys(table) {
		rows, err := tableSlice(typ, table[typ])
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			switch typ {
			case "localdir":
				t, err := parseLocalDir(row)
				if err != nil {
					return nil, err
				}
				targets = append(targets, t)
			default:
				return nil, fmt.Errorf("[[publish.%s]]: unknown publish target type %q", typ, typ)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Type != targets[j].Type {
			return targets[i].Type < targets[j].Type
		}
		return targets[i].Path < targets[j].Path
	})
	return targets, nil
}

func parseLocalDir(row map[string]any) (PublishTarget, error) {
	t := PublishTarget{Type: "localdir"}
	for _, k := range sortedKeys(row) {
		switch k {
		case "path":
			s, err := stringValue("publish.localdir", k, row[k])
			if err != nil {
				return t, err
			}
			cleaned := path.Clean(s)
			if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
				return t, fmt.Errorf("[[publish.localdir]]: path %q must stay within the workspace", s)
			}
			t.Path = cleaned
		default:
			return t, fmt.Errorf("[[publish.localdir]]: unknown key %q", k)
		}
	}
	if t.Path == "" {
		return t, fmt.Errorf("[[publish.localdir]]: missing required key %q", "path")
	}
	return t, nil
}

// tableSlice coerces a decoded [[array.of.tables]] value into a slice of
// tables, accepting the shapes BurntSushi may produce.
func tableSlice(section string, v any) ([]map[string]any, error) {
	switch arr := v.(type) {
	case []map[string]any:
		return arr, nil
	case []any:
		rows := make([]map[string]any, 0, len(arr))
		for _, e := range arr {
			row, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("[[publish.%s]] must be an array of tables", section)
			}
			rows = append(rows, row)
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("[[publish.%s]] must be an array of tables", section)
	}
}

func parseFiles(table map[string]any) ([]FileMapping, []SourceRef, error) {
	var files []FileMapping
	var excludes []SourceRef
	seen := make(map[string]string) // concrete dest -> source key, for duplicate detection

	for _, key := range sortedKeys(table) {
		// "exclude" with an array value is the reserved filter key, not a
		// source mapping. A string value means a source literally named
		// "exclude", which still maps normally — so the reservation never
		// shadows a real file.
		if key == "exclude" {
			if arr, isArr := table[key].([]any); isArr {
				ex, err := parseExclude(arr)
				if err != nil {
					return nil, nil, err
				}
				excludes = ex
				continue
			}
		}
		dest, ok := table[key].(string)
		if !ok {
			return nil, nil, fmt.Errorf("[files]: %q must map to a destination string", key)
		}
		src, err := parseSourceRef(key)
		if err != nil {
			return nil, nil, err
		}
		glob := hasGlobMeta(src.Path)
		cleaned, err := cleanFileDest(dest, glob)
		if err != nil {
			return nil, nil, err
		}
		// A glob's dest is a directory prefix; the archive paths it produces
		// are only known after expansion against the stage, so its collisions
		// are caught there (resolveFiles). Only concrete dests can be deduped
		// up front, before any build has run.
		if !glob {
			if prev, dup := seen[cleaned]; dup {
				return nil, nil, fmt.Errorf("[files]: %q and %q both map to %q", prev, key, cleaned)
			}
			seen[cleaned] = key
		}
		files = append(files, FileMapping{Source: src, Dest: cleaned})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Dest < files[j].Dest })
	return files, excludes, nil
}

// parseExclude parses [files].exclude: an array of source patterns (the same
// target:path syntax as [files] keys) naming staged files to drop before
// packing. Globs are allowed, so ":usr/bin/*trace" excludes a whole family.
func parseExclude(arr []any) ([]SourceRef, error) {
	var out []SourceRef
	for _, v := range arr {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("[files]: exclude must be an array of non-empty source patterns")
		}
		ref, err := parseSourceRef(s)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

func parseSourceRef(key string) (SourceRef, error) {
	if key == "" {
		return SourceRef{}, fmt.Errorf("[files]: sources must not be empty")
	}
	target, p, isRef := strings.Cut(key, ":")
	if !isRef {
		return SourceRef{Path: key}, nil
	}
	if target == "" {
		target = "main" // ":path" is sugar for "main:path"
	}
	if p == "" {
		return SourceRef{}, fmt.Errorf("[files]: source %q names no file inside target %s's stage", key, target)
	}
	cleaned := path.Clean(p)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return SourceRef{}, fmt.Errorf("[files]: source %q must stay inside the stage", key)
	}
	return SourceRef{Target: target, Path: cleaned}, nil
}

func cleanDest(dest string) (string, error) {
	if dest == "" {
		return "", fmt.Errorf("[files]: destinations must not be empty")
	}
	cleaned := path.Clean(dest)
	if path.IsAbs(cleaned) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("[files]: destination %q must be a relative path inside the package", dest)
	}
	return cleaned, nil
}

// cleanFileDest validates a [files] destination. A glob source's dest is a
// directory prefix that matches are rebased under, so "." — the archive root,
// i.e. "keep every staged path as-is" — is allowed there; a concrete source's
// dest must name a file, so "." is rejected (a file needs a name).
func cleanFileDest(dest string, glob bool) (string, error) {
	if dest == "" {
		return "", fmt.Errorf("[files]: destinations must not be empty")
	}
	cleaned := path.Clean(dest)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") ||
		(cleaned == "." && !glob) {
		return "", fmt.Errorf("[files]: destination %q must be a relative path inside the package", dest)
	}
	return cleaned, nil
}
