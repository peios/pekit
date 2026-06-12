package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/BurntSushi/toml"
)

// Target is a single build target.
type Target struct {
	Command string
}

// Config is a parsed pekit.toml.
type Config struct {
	Targets map[string]Target
}

// TargetNames returns the target names in sorted order.
func (c *Config) TargetNames() []string {
	names := make([]string, 0, len(c.Targets))
	for name := range c.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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

// ParseConfig parses pekit.toml source. [build] comes in exactly one of two
// shapes: bare ([build] with fields directly on it, sugar for a single target
// named "main") or named ([build.<name>] subtables). Mixing them is an error.
func ParseConfig(src string) (*Config, error) {
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}

	for key := range raw {
		if key != "build" {
			return nil, fmt.Errorf("unknown section %q", key)
		}
	}
	rawBuild, ok := raw["build"]
	if !ok {
		return nil, fmt.Errorf("missing [build] section")
	}
	buildTable, ok := rawBuild.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("[build] must be a table")
	}

	// Subtables under [build] are named targets; scalar keys are bare-form
	// target fields. The presence of both means the two shapes are mixed.
	var fields, subtables []string
	for key, val := range buildTable {
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
			"[build] mixes a bare target (key %q) with named targets ([build.%s]); use one shape or the other",
			fields[0], subtables[0])
	}

	cfg := &Config{Targets: make(map[string]Target)}

	if len(subtables) == 0 {
		target, err := parseTarget("build", buildTable)
		if err != nil {
			return nil, err
		}
		cfg.Targets["main"] = target
		return cfg, nil
	}

	for _, name := range subtables {
		target, err := parseTarget("build."+name, buildTable[name].(map[string]any))
		if err != nil {
			return nil, err
		}
		cfg.Targets[name] = target
	}
	return cfg, nil
}

func parseTarget(section string, table map[string]any) (Target, error) {
	var target Target
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		switch key {
		case "command":
			s, ok := table[key].(string)
			if !ok {
				return target, fmt.Errorf("[%s]: command must be a string", section)
			}
			if s == "" {
				return target, fmt.Errorf("[%s]: command must not be empty", section)
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
