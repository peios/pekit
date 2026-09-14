package pekit

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ReleaseConfig is workspace-owned. Its repository is separate from ordinary
// package publish targets, which may remain useful for bootstrap work.
type ReleaseConfig struct {
	Path           string
	Name           string
	SigningKey     string
	Environments   []string          // last environment produces the promoted archives
	ReferenceAllow map[string]string // explicit findings allowed only in non-publication environments
	Checks         map[string]ShellCommand
}

func parseReleaseConfig(path string, value any) (ReleaseConfig, error) {
	c := ReleaseConfig{Checks: map[string]ShellCommand{}}
	table, err := expectMap(path, "release", value)
	if err != nil {
		return c, err
	}
	for k, v := range table {
		switch k {
		case "path":
			c.Path, err = expectString(path, "release.path", v)
		case "name":
			c.Name, err = expectString(path, "release.name", v)
		case "signing_key":
			c.SigningKey, err = expectString(path, "release.signing_key", v)
		case "environments":
			c.Environments, err = expectStringSlice(path, "release.environments", v)
		case "reference_allow":
			var entries map[string]any
			entries, err = expectMap(path, "release.reference_allow", v)
			c.ReferenceAllow = map[string]string{}
			if err == nil {
				for rule, value := range entries {
					spec, ok := lintKeyIndex[rule]
					if !ok || spec.Param {
						return c, diagAt("unknown_key", path, "unknown release reference lint rule %q", rule)
					}
					reason, e := expectString(path, "release.reference_allow."+rule, value)
					if e != nil {
						return c, e
					}
					if strings.TrimSpace(reason) == "" {
						return c, diagAt("missing_reason", path, "reference lint allowance %s requires a reason", rule)
					}
					c.ReferenceAllow[rule] = reason
				}
			}
		case "checks":
			var checks map[string]any
			checks, err = expectMap(path, "release.checks", v)
			if err == nil {
				for name, command := range checks {
					if err = validateSelector("release check", name); err != nil {
						break
					}
					c.Checks[name], err = parseCommand(path, "release.checks."+name, command)
					if err == nil && c.Checks[name].Empty() {
						err = fmt.Errorf("release check %s is empty", name)
					}
					if err != nil {
						break
					}
				}
			}
		default:
			return c, diagAt("unknown_key", path, "unknown release key %q", k)
		}
		if err != nil {
			return c, err
		}
	}
	if c.Path == "" || c.Name == "" || c.SigningKey == "" || len(c.Environments) == 0 {
		return c, diagAt("missing_key", path, "release requires path, name, signing_key and environments")
	}
	if err = validateReleaseDestination(c.Path); err != nil {
		return c, err
	}
	if _, err = cleanRelPath(c.Path); err != nil {
		return c, err
	}
	seen := map[string]bool{}
	for _, e := range c.Environments {
		if err = validateSelector("environment", e); err != nil {
			return c, err
		}
		if seen[e] {
			return c, fmt.Errorf("duplicate release environment %s", e)
		}
		seen[e] = true
	}
	return c, nil
}
func guardProductionPublish(ws *WorkspaceConfig, ops publishPlan) error {
	if ws == nil || ws.Release.Path == "" {
		return nil
	}
	target := filepath.Join(ws.Root, ws.Release.Path)
	for _, op := range ops.Peipkg {
		if op.Dir == target {
			return diag("qualification_required", "production repository requires `pekit release`; ordinary publish has no candidate qualification")
		}
	}
	for _, op := range ops.LocalDir {
		if withinDirectory(target, op.Dest) {
			return diag("qualification_required", "ordinary publish cannot write into the production repository")
		}
	}
	return nil
}

func validateReleaseDestination(path string) error {
	clean, err := cleanRelPath(path)
	if err != nil || clean == "." || clean != path || path == ".pekit" || withinDirectory(".pekit", path) {
		return fmt.Errorf("release path must name a separate repository directory")
	}
	return nil
}
