package main

import (
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/BurntSushi/toml"
)

// knownSections are the verbs pekit understands. Anything else in
// pekit.toml is rejected so typos stay loud.
var knownSections = []string{"build", "install"}

// Target is a single runnable target within a section.
type Target struct {
	Command string
}

// Config is a parsed pekit.toml: verb -> target name -> target.
type Config struct {
	Sections map[string]map[string]Target
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

	cfg := &Config{Sections: make(map[string]map[string]Target)}

	sections := make([]string, 0, len(raw))
	for key := range raw {
		sections = append(sections, key)
	}
	sort.Strings(sections)

	for _, section := range sections {
		if !slices.Contains(knownSections, section) {
			return nil, fmt.Errorf("unknown section %q", section)
		}
		table, ok := raw[section].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("[%s] must be a table", section)
		}
		targets, err := parseSection(section, table)
		if err != nil {
			return nil, err
		}
		cfg.Sections[section] = targets
	}
	return cfg, nil
}

func parseSection(section string, table map[string]any) (map[string]Target, error) {
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

	targets := make(map[string]Target)

	if len(subtables) == 0 {
		target, err := parseTarget(section, table)
		if err != nil {
			return nil, err
		}
		targets["main"] = target
		return targets, nil
	}

	for _, name := range subtables {
		target, err := parseTarget(section+"."+name, table[name].(map[string]any))
		if err != nil {
			return nil, err
		}
		targets[name] = target
	}
	return targets, nil
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

func sortedNames(targets map[string]Target) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
