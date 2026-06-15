package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/bmatcuk/doublestar/v4"
)

// Multipack is a parsed [multipack] section: a single package.pekit.toml
// fans out into one package per enum value, with {{multipack}} bound to that
// value across the whole file (paths, name, dependencies, ...). It is an axis
// orthogonal to {{version}} and to prefixed members — a standalone file or a
// member file may carry one. Suffix, if set, is appended to the package name
// with {{multipack}} substituted, so the N packages get distinct names.
//
// The enum comes in two shapes (exactly one of Values / Files is set):
// Values is a literal array (the stringified entries, in document order);
// Files derives the enum at resolve time by enumerating staged files and
// capturing a value from each — for axes too large or too version-dependent
// to write by hand (glibc's hundreds of locales).
type Multipack struct {
	Values []string
	Files  *MultipackFiles
	Suffix string
}

// MultipackFiles is a derived enum (enum.files): glob Source against its
// build target's stage, then take capture group 1 of Regex over each match's
// basename as a value. The distinct values (sorted) become the enum, so many
// files collapsing to one code (en_US, en_GB → "en") yield one package.
type MultipackFiles struct {
	Source SourceRef
	Regex  *regexp.Regexp
}

// parseMultipack extracts and validates the [multipack] section from a decoded
// (and version-rendered) package table, returning nil when the section is
// absent. A derived enum's values are NOT resolved here (that needs a built
// stage — see resolveMultipackValues); only its path and regex are validated.
// The table is read only — the caller strips the directive before parsing each
// instance as an ordinary package.
func parseMultipack(raw map[string]any) (*Multipack, error) {
	v, ok := raw["multipack"]
	if !ok {
		return nil, nil
	}
	table, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("[multipack] must be a table")
	}

	mp := &Multipack{}
	for _, key := range sortedKeys(table) {
		switch key {
		case "enum":
			if err := mp.parseEnum(table[key]); err != nil {
				return nil, err
			}
		case "suffix":
			s, err := stringValue("multipack", key, table[key])
			if err != nil {
				return nil, err
			}
			mp.Suffix = s
		default:
			return nil, fmt.Errorf("[multipack]: unknown key %q", key)
		}
	}
	if mp.Values == nil && mp.Files == nil {
		return nil, fmt.Errorf("[multipack]: missing required key %q", "enum")
	}
	return mp, nil
}

// parseEnum dispatches on the enum's shape: an array is a literal enum, a
// table is the derived enum.files form. Any other type is rejected.
func (mp *Multipack) parseEnum(v any) error {
	switch e := v.(type) {
	case []any:
		seen := make(map[string]bool)
		for _, item := range e {
			s, err := multipackValue(item)
			if err != nil {
				return err
			}
			if seen[s] {
				return fmt.Errorf("[multipack]: duplicate enum value %q", s)
			}
			seen[s] = true
			mp.Values = append(mp.Values, s)
		}
		if len(mp.Values) == 0 {
			return fmt.Errorf("[multipack]: enum must be a non-empty array of strings or integers")
		}
		return nil
	case map[string]any:
		return mp.parseEnumFiles(e)
	default:
		return fmt.Errorf("[multipack]: enum must be an array of values or an enum.files table")
	}
}

// parseEnumFiles validates the enum.files = { path, regex } table. path is a
// [files]-style source (target:glob); regex must compile and carry at least
// one capture group, whose match is the value. Resolution is deferred — the
// stage may not be built yet.
func (mp *Multipack) parseEnumFiles(enum map[string]any) error {
	for _, k := range sortedKeys(enum) {
		if k != "files" {
			return fmt.Errorf("[multipack]: unknown enum key %q (use a literal array or enum.files)", k)
		}
	}
	files, ok := enum["files"].(map[string]any)
	if !ok {
		return fmt.Errorf("[multipack]: enum.files must be a table of { path, regex }")
	}

	var pathStr, regexStr string
	for _, k := range sortedKeys(files) {
		switch k {
		case "path":
			s, err := stringValue("multipack.enum.files", k, files[k])
			if err != nil {
				return err
			}
			pathStr = s
		case "regex":
			s, err := stringValue("multipack.enum.files", k, files[k])
			if err != nil {
				return err
			}
			regexStr = s
		default:
			return fmt.Errorf("[multipack]: enum.files: unknown key %q", k)
		}
	}
	if pathStr == "" {
		return fmt.Errorf("[multipack]: enum.files: missing required key %q", "path")
	}
	if regexStr == "" {
		return fmt.Errorf("[multipack]: enum.files: missing required key %q", "regex")
	}

	src, err := parseSourceRef(pathStr)
	if err != nil {
		return err
	}
	re, err := regexp.Compile(regexStr)
	if err != nil {
		return fmt.Errorf("[multipack]: enum.files: regex %q is not a valid regexp: %w", regexStr, err)
	}
	if re.NumSubexp() < 1 {
		return fmt.Errorf("[multipack]: enum.files: regex %q needs a capture group naming the value", regexStr)
	}
	mp.Files = &MultipackFiles{Source: src, Regex: re}
	return nil
}

// enumerateMultipackValues globs src under root (the build target's stage, or
// the literal root) and returns the distinct, sorted capture-group-1 values of
// re over each match's basename. Entries the regex doesn't match are skipped
// (a locale dir like "C" has no language code); matching nothing at all is an
// error, like an empty literal enum.
func enumerateMultipackValues(root string, src SourceRef, re *regexp.Regexp) ([]string, error) {
	base, pattern := doublestar.SplitPattern(src.Path)
	baseAbs, err := filepath.Abs(filepath.Join(root, base))
	if err != nil {
		return nil, err
	}
	matches, err := doublestar.Glob(os.DirFS(baseAbs), pattern, doublestar.WithNoFollow())
	if err != nil {
		return nil, fmt.Errorf("[multipack]: enum.files path %q: %w", src, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("[multipack]: enum.files path %q matched nothing under %s", src, baseAbs)
	}

	set := make(map[string]bool)
	for _, m := range matches {
		sub := re.FindStringSubmatch(path.Base(m))
		if sub == nil || sub[1] == "" {
			continue
		}
		set[sub[1]] = true
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("[multipack]: enum.files regex %q matched none of the %d entries under %s", re.String(), len(matches), baseAbs)
	}
	values := make([]string, 0, len(set))
	for v := range set {
		values = append(values, v)
	}
	sort.Strings(values)
	return values, nil
}

// multipackValue stringifies one enum entry. Integers (the common case —
// enum = [1, 2, 3]) render without decoration; strings pass through. Other
// scalar types are rejected so a stray float or bool fails loudly.
func multipackValue(e any) (string, error) {
	switch t := e.(type) {
	case string:
		if t == "" {
			return "", fmt.Errorf("[multipack]: enum values must be non-empty")
		}
		return t, nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	default:
		return "", fmt.Errorf("[multipack]: enum values must be strings or integers (got %T)", e)
	}
}

// substituteMultipack replaces every {{multipack}} placeholder in s with value,
// leaving any other {{...}} untouched. By the time this runs the version pass
// has already resolved (or errored on) every other variable, so multipack is
// the only placeholder that can legitimately remain.
func substituteMultipack(s, value string) string {
	return templateVar.ReplaceAllStringFunc(s, func(match string) string {
		if templateVar.FindStringSubmatch(match)[1] == "multipack" {
			return value
		}
		return match
	})
}

// renderMultipackValue returns a deep copy of a decoded TOML value with
// {{multipack}} substituted to value throughout — map keys as well as values,
// and array elements recursively. Keys are rendered because [files] sources
// (":locale/{{multipack}}/...") live there; numbers and bools pass through
// unchanged. The input is never mutated, so one merged table expands cleanly
// into each instance.
func renderMultipackValue(v any, value string) (any, error) {
	switch t := v.(type) {
	case string:
		return substituteMultipack(t, value), nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rk := substituteMultipack(k, value)
			if _, dup := out[rk]; dup {
				return nil, fmt.Errorf("multipack %q: keys collide on %q after substitution", value, rk)
			}
			rv, err := renderMultipackValue(val, value)
			if err != nil {
				return nil, err
			}
			out[rk] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			re, err := renderMultipackValue(e, value)
			if err != nil {
				return nil, err
			}
			out[i] = re
		}
		return out, nil
	default:
		return v, nil
	}
}

// containsMultipackVar reports whether a {{multipack}} placeholder appears
// anywhere in a decoded value (keys, values, array elements). Used to reject a
// recipe that references {{multipack}} but defines no [multipack] section,
// which would otherwise ship a literal "{{multipack}}" in a path.
func containsMultipackVar(v any) bool {
	switch t := v.(type) {
	case string:
		return multipackInString(t)
	case map[string]any:
		for k, val := range t {
			if multipackInString(k) || containsMultipackVar(val) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if containsMultipackVar(e) {
				return true
			}
		}
	}
	return false
}

func multipackInString(s string) bool {
	for _, m := range templateVar.FindAllStringSubmatch(s, -1) {
		if m[1] == "multipack" {
			return true
		}
	}
	return false
}

// withoutKey returns a shallow copy of m with key removed, so the [multipack]
// directive can be dropped before an instance is parsed as an ordinary package
// without mutating the shared merged table.
func withoutKey(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
}
