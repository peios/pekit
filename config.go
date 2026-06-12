package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/BurntSushi/toml"
)

// Target is a single runnable target within a command section.
type Target struct {
	Command string
}

// Package is a single package target.
type Package struct {
	Format string
}

// Config is a parsed pekit.toml.
type Config struct {
	// OutDir is the pekit-managed staging directory; empty when unset.
	// Each build target gets OutDir/<name>, exported as $PEKIT_OUT.
	OutDir string
	// ClearOut wipes a target's staging dir before its build. Default true.
	ClearOut bool
	// Commands holds the command-running verbs: verb -> target name -> target.
	Commands map[string]map[string]Target
	// Packages holds [package] targets; nil when the section is absent.
	Packages map[string]Package
}

// LoadConfig reads and parses a pekit.toml file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(string(data))
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
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}

	cfg := &Config{ClearOut: true, Commands: make(map[string]map[string]Target)}
	clearOutSet := false

	keys := sortedKeys(raw)
	for _, key := range keys {
		if table, isTable := raw[key].(map[string]any); isTable {
			switch key {
			case "build", "install":
				targets, err := parseSection(key, table, parseTarget)
				if err != nil {
					return nil, err
				}
				cfg.Commands[key] = targets
			case "package":
				packages, err := parseSection(key, table, parsePackage)
				if err != nil {
					return nil, err
				}
				cfg.Packages = packages
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
			cfg.OutDir = s
		case "clearOut":
			b, ok := raw[key].(bool)
			if !ok {
				return nil, fmt.Errorf("clearOut must be a boolean")
			}
			cfg.ClearOut = b
			clearOutSet = true
		case "build", "install", "package":
			return nil, fmt.Errorf("[%s] must be a table", key)
		default:
			return nil, fmt.Errorf("unknown root key %q", key)
		}
	}

	if clearOutSet && cfg.OutDir == "" {
		return nil, fmt.Errorf("clearOut requires outDir to be set")
	}
	return cfg, nil
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

func parsePackage(section string, table map[string]any) (Package, error) {
	var pkg Package
	for _, key := range sortedKeys(table) {
		switch key {
		case "format":
			s, err := stringValue(section, key, table[key])
			if err != nil {
				return pkg, err
			}
			pkg.Format = s
		default:
			return pkg, fmt.Errorf("[%s]: unknown key %q", section, key)
		}
	}
	if pkg.Format == "" {
		return pkg, fmt.Errorf("[%s]: missing required key %q", section, "format")
	}
	return pkg, nil
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
