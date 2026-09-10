package pekit

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// lint.pekit.toml enables and parameterises pekit's built-in lint rules.
// pekit ships no rules on by default: a rule runs only when some file names
// it, so a tree with no lint file has nothing to lint.
//
// Files are found walking up from the recipe directory to the workspace root
// (a recipe outside a workspace reads only its own), plus the source root of
// a delegated recipe. They merge outermost first, nearest winning per key,
// with one restriction: a nearer file may re-parameterise a rule but may
// not switch it off. Switching a rule off goes through [allow], with a
// reason, so every exemption is written down where a reviewer can find it.
// The source root's file merges FIRST — a project may hold itself to a
// higher bar than the distribution packaging it, never a lower one.

const lintFileName = "lint.pekit.toml"

// lintValueKind is the value shape a lint key accepts.
type lintValueKind int

const (
	lintBool            lintValueKind = iota // true
	lintString                               // one of Enum
	lintBoolOrString                         // true, or one of Enum
	lintBoolOrInt                            // true, or an integer parameter
	lintStrings                              // array of strings
	lintBoolOrStrings                        // true, or an array of strings
	lintStringOrStrings                      // a string or an array of strings
	lintTemplate                             // a free string with {{name}} etc.
)

// lintKeySpec describes one key a lint file may contain: a rule (which can
// produce findings and be named in [allow]) or a parameter that shapes how
// a rule runs.
type lintKeySpec struct {
	ID      string
	Kind    lintValueKind
	Enum    []string
	Param   bool // a parameter, not a rule
	Payload bool // needs a staged payload, so runs only with a version
}

var lintKeys = []lintKeySpec{
	// [package]
	{ID: "package.name.style", Kind: lintString, Enum: []string{"reverse-dns", "lowercase"}},
	{ID: "package.license", Kind: lintBoolOrString, Enum: []string{"spdx"}},
	{ID: "package.license_class", Kind: lintBool},
	{ID: "package.homepage", Kind: lintBoolOrString, Enum: []string{"https"}},
	{ID: "package.description", Kind: lintBoolOrInt},
	{ID: "package.dependencies", Kind: lintString, Enum: []string{"consistent"}},
	{ID: "package.architecture", Kind: lintString, Enum: []string{"consistent"}, Payload: true},
	{ID: "package.noarch", Kind: lintTemplate, Param: true},
	// [source]
	{ID: "source.reproducible", Kind: lintBool},
	{ID: "source.discovery", Kind: lintBool},
	{ID: "source.versions.floor", Kind: lintBool},
	{ID: "source.ref", Kind: lintString, Enum: []string{"immutable"}},
	{ID: "source.url.scheme", Kind: lintString, Enum: []string{"https"}},
	{ID: "source.lock", Kind: lintBool},
	{ID: "source.signature.required", Kind: lintBool},
	{ID: "source.signature.fingerprint", Kind: lintString, Enum: []string{"full"}},
	{ID: "source.signature.keys", Kind: lintBool},
	{ID: "source.patches.headers", Kind: lintBool},
	// [build]
	{ID: "build.dependencies.providers", Kind: lintStrings},
	{ID: "build.test", Kind: lintBool},
	// [payload]
	{ID: "payload.junk", Kind: lintBoolOrStrings, Payload: true},
	{ID: "payload.scripts", Kind: lintBool, Payload: true},
	{ID: "payload.interpreters", Kind: lintStrings, Param: true},
	{ID: "payload.symlinks.dangling", Kind: lintString, Enum: []string{"forbidden"}, Payload: true},
	{ID: "payload.symlinks.absolute", Kind: lintString, Enum: []string{"forbidden"}, Payload: true},
	{ID: "payload.filenames", Kind: lintString, Enum: []string{"portable"}, Payload: true},
	{ID: "payload.special_files", Kind: lintString, Enum: []string{"forbidden"}, Payload: true},
	{ID: "payload.manpages.required", Kind: lintBool, Payload: true},
	{ID: "payload.manpages.compression", Kind: lintString, Enum: []string{"gzip", "none"}, Payload: true},
	{ID: "payload.pkgconfig", Kind: lintBool, Payload: true},
	{ID: "payload.license_file", Kind: lintTemplate, Payload: true},
	{ID: "payload.dirs.bin", Kind: lintStrings, Param: true},
	{ID: "payload.dirs.lib", Kind: lintStrings, Param: true},
	{ID: "payload.dirs.man", Kind: lintStrings, Param: true},
	// [split]
	{ID: "split.devel.packages", Kind: lintStringOrStrings, Payload: true},
	{ID: "split.devel.files", Kind: lintStrings, Param: true},
	// [elf]
	{ID: "elf.pie", Kind: lintBool, Payload: true},
	{ID: "elf.stack", Kind: lintString, Enum: []string{"non-exec"}, Payload: true},
	{ID: "elf.relro", Kind: lintString, Enum: []string{"full", "partial"}, Payload: true},
	{ID: "elf.relr", Kind: lintBool, Payload: true},
	{ID: "elf.cet", Kind: lintBool, Payload: true},
	{ID: "elf.rpath", Kind: lintString, Enum: []string{"forbidden"}, Payload: true},
	{ID: "elf.textrel", Kind: lintString, Enum: []string{"forbidden"}, Payload: true},
	{ID: "elf.stripped", Kind: lintBool, Payload: true},
	{ID: "elf.debuginfo", Kind: lintTemplate, Payload: true},
	{ID: "elf.build_paths", Kind: lintString, Enum: []string{"forbidden"}, Payload: true},
	{ID: "elf.soname", Kind: lintBool, Payload: true},
}

var lintKeyIndex = func() map[string]lintKeySpec {
	m := make(map[string]lintKeySpec, len(lintKeys))
	for _, k := range lintKeys {
		m[k.ID] = k
	}
	return m
}()

// lintAllow is one [allow] entry: the file that granted it and why.
type lintAllow struct {
	Reason string
	Path   string
}

// LintConfig is the merged lint configuration for one recipe.
type LintConfig struct {
	Files  []string
	values map[string]any
	origin map[string]string // key -> file that set the effective value
	Allow  map[string]lintAllow
}

// Enabled reports whether a rule or parameter has any value: `false` and an
// absent key are both "off".
func (c LintConfig) Enabled(id string) bool {
	v, ok := c.values[id]
	if !ok {
		return false
	}
	if b, isBool := v.(bool); isBool {
		return b
	}
	return true
}

// Str returns a rule's string value, "" when it is absent or boolean.
func (c LintConfig) Str(id string) string {
	s, _ := c.values[id].(string)
	return s
}

// Int returns a rule's integer value and whether one was given.
func (c LintConfig) Int(id string) (int, bool) {
	switch v := c.values[id].(type) {
	case int64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// Strings returns a rule's array value; a lone string is a one-element
// array, and a boolean or absent key is nil.
func (c LintConfig) Strings(id string) []string {
	switch v := c.values[id].(type) {
	case []string:
		return v
	case string:
		return []string{v}
	}
	return nil
}

// StringsOr returns the configured array or, when the key is absent or a
// bare `true`, the given default.
func (c LintConfig) StringsOr(id string, def []string) []string {
	if s := c.Strings(id); s != nil {
		return s
	}
	return def
}

// lintFileValues is one parsed lint file: its rule and parameter leaves by
// dotted key, and its [allow] table.
type lintFileValues struct {
	Path   string
	Values map[string]any
	Allow  map[string]string
}

// loadLintFile parses one lint.pekit.toml. Every key must be a known rule
// or parameter with a value of the right shape, so the key table is
// complete and a typo is an error rather than a silently unrun rule.
func loadLintFile(path string) (lintFileValues, error) {
	raw, err := loadTOML(path)
	if err != nil {
		return lintFileValues{}, err
	}
	out := lintFileValues{Path: path, Values: map[string]any{}, Allow: map[string]string{}}
	if v, ok := raw["allow"]; ok {
		table, err := expectMap(path, "allow", v)
		if err != nil {
			return lintFileValues{}, err
		}
		for id, reasonVal := range table {
			spec, known := lintKeyIndex[id]
			if !known || spec.Param {
				return lintFileValues{}, diagAt("unknown_key", path, "allow.%q does not name a lint rule", id)
			}
			reason, err := expectString(path, "allow."+id, reasonVal)
			if err != nil {
				return lintFileValues{}, err
			}
			if strings.TrimSpace(reason) == "" {
				return lintFileValues{}, diagAt("missing_reason", path, "allow.%q needs a reason: say why the rule does not apply here", id)
			}
			out.Allow[id] = reason
		}
		delete(raw, "allow")
	}
	if err := flattenLintTable(path, "", raw, out.Values); err != nil {
		return lintFileValues{}, err
	}
	return out, nil
}

// flattenLintTable walks nested tables into dotted leaf keys, validating
// each leaf against the key table.
func flattenLintTable(path, prefix string, table map[string]any, out map[string]any) error {
	for key, value := range table {
		id := key
		if prefix != "" {
			id = prefix + "." + key
		}
		spec, known := lintKeyIndex[id]
		if !known {
			if sub, isTable := value.(map[string]any); isTable {
				if err := flattenLintTable(path, id, sub, out); err != nil {
					return err
				}
				continue
			}
			return diagAt("unknown_key", path, "unknown lint key %q", id)
		}
		v, err := checkLintValue(path, spec, value)
		if err != nil {
			return err
		}
		out[id] = v
	}
	return nil
}

// checkLintValue coerces a TOML value into the shape the key accepts.
func checkLintValue(path string, spec lintKeySpec, value any) (any, error) {
	bad := func(want string) error {
		return diagAt("invalid_value", path, "%s must be %s", spec.ID, want)
	}
	enumText := func() string {
		quoted := make([]string, 0, len(spec.Enum))
		for _, e := range spec.Enum {
			quoted = append(quoted, fmt.Sprintf("%q", e))
		}
		return strings.Join(quoted, " or ")
	}
	inEnum := func(s string) bool {
		for _, e := range spec.Enum {
			if s == e {
				return true
			}
		}
		return false
	}
	switch spec.Kind {
	case lintBool:
		b, ok := value.(bool)
		if !ok {
			return nil, bad("true or false")
		}
		return b, nil
	case lintString:
		s, ok := value.(string)
		if !ok || !inEnum(s) {
			return nil, bad(enumText())
		}
		return s, nil
	case lintBoolOrString:
		switch v := value.(type) {
		case bool:
			return v, nil
		case string:
			if inEnum(v) {
				return v, nil
			}
		}
		return nil, bad("true, false, or " + enumText())
	case lintBoolOrInt:
		switch v := value.(type) {
		case bool:
			return v, nil
		case int64:
			if v <= 0 {
				return nil, bad("a positive integer")
			}
			return v, nil
		}
		return nil, bad("true, false, or a positive integer")
	case lintStrings:
		s, err := expectStringSlice(path, spec.ID, value)
		if err != nil {
			return nil, err
		}
		return s, nil
	case lintBoolOrStrings:
		if b, ok := value.(bool); ok {
			return b, nil
		}
		s, err := expectStringSlice(path, spec.ID, value)
		if err != nil {
			return nil, bad("true, false, or an array of strings")
		}
		return s, nil
	case lintStringOrStrings:
		if s, ok := value.(string); ok {
			return []string{s}, nil
		}
		s, err := expectStringSlice(path, spec.ID, value)
		if err != nil {
			return nil, bad("a string or an array of strings")
		}
		return s, nil
	case lintTemplate:
		s, ok := value.(string)
		if !ok || s == "" {
			return nil, bad("a non-empty string")
		}
		return s, nil
	}
	return nil, bad("a supported value")
}

// lintFilePaths lists the lint files that apply to a recipe, outermost
// first: the delegated source root (when known), then the workspace root
// down to the recipe directory. Outside a workspace only the recipe's own
// file is read.
func lintFilePaths(recipeRoot string, workspace *WorkspaceConfig, sourceRoot string) []string {
	var paths []string
	if sourceRoot != "" && sourceRoot != recipeRoot {
		if p := filepath.Join(sourceRoot, lintFileName); fileExists(p) {
			paths = append(paths, p)
		}
	}
	var chain []string
	dir := recipeRoot
	for {
		chain = append(chain, dir)
		if workspace == nil || dir == workspace.Root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// chain is recipe -> workspace; the merge wants workspace -> recipe.
	for i := len(chain) - 1; i >= 0; i-- {
		if p := filepath.Join(chain[i], lintFileName); fileExists(p) {
			paths = append(paths, p)
		}
	}
	return paths
}

// loadLintConfig finds and merges the lint files for a recipe.
func loadLintConfig(recipeRoot string, workspace *WorkspaceConfig, sourceRoot string) (LintConfig, error) {
	cfg := LintConfig{values: map[string]any{}, origin: map[string]string{}, Allow: map[string]lintAllow{}}
	for _, path := range lintFilePaths(recipeRoot, workspace, sourceRoot) {
		file, err := loadLintFile(path)
		if err != nil {
			return LintConfig{}, err
		}
		if err := cfg.merge(file); err != nil {
			return LintConfig{}, err
		}
	}
	return cfg, nil
}

// merge layers one file over the configuration so far. A nearer file may
// change a rule's parameters or a parameter's value freely; setting a rule
// that an outer file enabled to `false` is refused.
func (c *LintConfig) merge(file lintFileValues) error {
	c.Files = append(c.Files, file.Path)
	keys := make([]string, 0, len(file.Values))
	for k := range file.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := file.Values[key]
		spec := lintKeyIndex[key]
		if b, isBool := value.(bool); isBool && !b && !spec.Param {
			if prev, ok := c.values[key]; ok && lintValueEnabled(prev) {
				return diagAt("lint_rule_disabled", file.Path,
					"%s switches off a rule that %s enables; a nearer file cannot disable a rule — add it to [allow] with a reason instead",
					key, c.origin[key])
			}
		}
		c.values[key] = value
		c.origin[key] = file.Path
	}
	for id, reason := range file.Allow {
		c.Allow[id] = lintAllow{Reason: reason, Path: file.Path}
	}
	return nil
}

func lintValueEnabled(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}

// EnabledRules lists the rule IDs (not parameters) the configuration turns
// on, in table order.
func (c LintConfig) EnabledRules() []string {
	var out []string
	for _, k := range lintKeys {
		if !k.Param && c.Enabled(k.ID) {
			out = append(out, k.ID)
		}
	}
	return out
}
