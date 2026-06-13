package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/BurntSushi/toml"
	semver "github.com/Masterminds/semver/v3"
)

// Target is a single runnable target within a command section.
type Target struct {
	Command string
}

// EnvVar is one [env] entry. Order matters: later values may reference
// earlier ones, so Config.Env preserves document order.
type EnvVar struct {
	Name  string
	Value string
}

// Config is a parsed pekit.toml.
type Config struct {
	// Env is exported (in order) at the top of every command script.
	Env []EnvVar
	// OutDir is the pekit-managed staging directory; empty when unset.
	// Each build target gets OutDir/<name>, exported as $PEKIT_OUT.
	OutDir string
	// ClearOut wipes a target's staging dir before its build. Default true.
	ClearOut bool
	// Commands holds the command-running verbs: verb -> target name -> target.
	Commands map[string]map[string]Target
	// Source, when set, is upstream source the build fetches and builds
	// in instead of the project directory; nil = the cwd is the source.
	Source *Source
}

// Source is a [source] block: upstream the build checks out before
// running. The checkout lands in OutDir/source/<rev> and becomes the
// build's working directory.
type Source struct {
	Git string
	Rev string
	// Versions, when set, is a semver constraint bounding which upstream
	// versions this recipe builds. The resolved --version set (including
	// an enumerated "*") is intersected with it, so tags the recipe can't
	// build (e.g. releases predating its packaging files) are skipped
	// rather than attempted. Empty = no bound.
	Versions string
}

// LoadConfig reads, templates, and parses a pekit.toml file.
func LoadConfig(path string, ver *Version) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rendered, err := renderTemplate(string(data), ver)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg, err := ParseConfig(rendered)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig parses pekit.toml source. Every section comes in exactly one
// of two shapes: bare ([build] with fields directly on it, sugar for a
// single target named "main") or named ([build.<name>] subtables). Mixing
// the shapes within a section is an error. Sections are independent — one
// may be bare while another is named.
func ParseConfig(src string) (*Config, error) {
	var raw map[string]any
	md, err := toml.Decode(src, &raw)
	if err != nil {
		return nil, err
	}

	cfg := &Config{ClearOut: true, Commands: make(map[string]map[string]Target)}
	clearOutSet := false

	keys := sortedKeys(raw)
	for _, key := range keys {
		if table, isTable := raw[key].(map[string]any); isTable {
			switch key {
			case "build", "test", "install", "clean":
				targets, err := parseSection(key, table, parseTarget)
				if err != nil {
					return nil, err
				}
				cfg.Commands[key] = targets
			case "package":
				return nil, fmt.Errorf("[package] has moved to package.pekit.toml")
			case "env":
				env, err := parseEnv(table, md)
				if err != nil {
					return nil, err
				}
				cfg.Env = env
			case "source":
				source, err := parseSource(table)
				if err != nil {
					return nil, err
				}
				cfg.Source = source
			default:
				return nil, fmt.Errorf("unknown section %q", key)
			}
			continue
		}

		switch key {
		case "outDir":
			s, ok := raw[key].(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("outDir must be a non-empty string")
			}
			// pekit clean does RemoveAll(outDir); these values would
			// make that "delete the project" (or worse).
			switch filepath.Clean(s) {
			case ".", "..", "/":
				return nil, fmt.Errorf("outDir must name a subdirectory (got %q)", s)
			}
			cfg.OutDir = s
		case "clearOut":
			b, ok := raw[key].(bool)
			if !ok {
				return nil, fmt.Errorf("clearOut must be a boolean")
			}
			cfg.ClearOut = b
			clearOutSet = true
		case "build", "test", "install", "clean", "package", "env", "source":
			return nil, fmt.Errorf("[%s] must be a table", key)
		default:
			return nil, fmt.Errorf("unknown root key %q", key)
		}
	}

	if clearOutSet && cfg.OutDir == "" {
		return nil, fmt.Errorf("clearOut requires outDir to be set")
	}
	if cfg.Source != nil && cfg.OutDir == "" {
		return nil, fmt.Errorf("[source] requires outDir to be set (the checkout lands under it)")
	}
	return cfg, nil
}

// parseSource parses a [source] block. git and rev are both required;
// rev is taken verbatim (tag, commit, or any git-checkout-able ref) —
// reproducibility comes from passing an immutable rev, not from pekit
// policing it, since in practice an orchestrator injects a concrete one.
func parseSource(table map[string]any) (*Source, error) {
	var src Source
	for _, key := range sortedKeys(table) {
		switch key {
		case "git":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			src.Git = s
		case "rev":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			src.Rev = s
		case "versions":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			if _, err := semver.NewConstraint(s); err != nil {
				return nil, fmt.Errorf("[source]: versions %q is not a valid semver constraint: %w", s, err)
			}
			src.Versions = s
		default:
			return nil, fmt.Errorf("[source]: unknown key %q", key)
		}
	}
	if src.Git == "" {
		return nil, fmt.Errorf("[source]: missing required key %q", "git")
	}
	if src.Rev == "" {
		return nil, fmt.Errorf("[source]: missing required key %q", "rev")
	}
	return &src, nil
}

// parseSection applies the shared shape rules (bare vs named, never mixed)
// and parses each target table with parseOne.
func parseSection[T any](section string, table map[string]any, parseOne func(string, map[string]any) (T, error)) (map[string]T, error) {
	// Subtables are named targets; scalar keys are bare-form target fields.
	// The presence of both means the two shapes are mixed.
	var fields, subtables []string
	for key, val := range table {
		if _, isTable := val.(map[string]any); isTable {
			subtables = append(subtables, key)
		} else {
			fields = append(fields, key)
		}
	}
	sort.Strings(fields)
	sort.Strings(subtables)

	if len(fields) > 0 && len(subtables) > 0 {
		return nil, fmt.Errorf(
			"[%s] mixes a bare target (key %q) with named targets ([%s.%s]); use one shape or the other",
			section, fields[0], section, subtables[0])
	}

	targets := make(map[string]T)

	if len(subtables) == 0 {
		target, err := parseOne(section, table)
		if err != nil {
			return nil, err
		}
		targets["main"] = target
		return targets, nil
	}

	for _, name := range subtables {
		target, err := parseOne(section+"."+name, table[name].(map[string]any))
		if err != nil {
			return nil, err
		}
		targets[name] = target
	}
	return targets, nil
}

func parseTarget(section string, table map[string]any) (Target, error) {
	var target Target
	for _, key := range sortedKeys(table) {
		switch key {
		case "command":
			s, err := stringValue(section, key, table[key])
			if err != nil {
				return target, err
			}
			target.Command = s
		default:
			return target, fmt.Errorf("[%s]: unknown key %q", section, key)
		}
	}
	if target.Command == "" {
		return target, fmt.Errorf("[%s]: missing required key %q", section, "command")
	}
	return target, nil
}

// envNameRe matches valid sh identifiers. Anything else in an export
// line would be a syntax error at best and script injection at worst.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseEnv parses [env] preserving document order (TOML maps lose it,
// so order comes from the decoder metadata). Values are verbatim sh
// double-quoted-string contents; only names are validated.
func parseEnv(table map[string]any, md toml.MetaData) ([]EnvVar, error) {
	var env []EnvVar
	for _, key := range md.Keys() {
		if len(key) != 2 || key[0] != "env" {
			continue
		}
		name := key[1]
		if !envNameRe.MatchString(name) {
			return nil, fmt.Errorf("[env]: invalid variable name %q", name)
		}
		if name == "PEKIT_OUT" {
			return nil, fmt.Errorf("[env]: PEKIT_OUT is set by pekit and cannot be overridden")
		}
		val, ok := table[name].(string)
		if !ok {
			return nil, fmt.Errorf("[env]: %s must be a string", name)
		}
		env = append(env, EnvVar{Name: name, Value: val})
	}
	return env, nil
}

func stringValue(section, key string, val any) (string, error) {
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("[%s]: %s must be a string", section, key)
	}
	if s == "" {
		return "", fmt.Errorf("[%s]: %s must not be empty", section, key)
	}
	return s, nil
}

func sortedKeys(table map[string]any) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNames[T any](targets map[string]T) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
