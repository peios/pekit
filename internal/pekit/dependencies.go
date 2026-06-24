package pekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/peios/peipkg/pack"
)

type DependencyPayload struct {
	Command      string                       `json:"command"`
	Target       string                       `json:"target"`
	Provider     string                       `json:"provider"`
	Version      string                       `json:"version,omitempty"`
	Dependencies map[string]string            `json:"dependencies"`
	AllProviders map[string]map[string]string `json:"all_providers"`
}

func dependencyManagedEnv(source SourceState, version Version, target TargetConfig, provider string) (map[string]string, error) {
	payload, err := renderDependencyPayload(version, target, provider)
	if err != nil {
		return nil, err
	}
	path := dependencyFilePath(source, target)
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, wrapDiag("dependency_payload", "encode dependency payload", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, wrapDiag("mkdir", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, wrapDiag("write_dependency_payload", path, err)
	}
	return map[string]string{
		"PEKIT_DEPENDENCIES_FILE":   path,
		"PEKIT_DEPENDENCY_PROVIDER": provider,
		"PEKIT_DEPENDENCIES":        dependencyList(payload.Dependencies),
	}, nil
}

func dependencyFilePath(source SourceState, target TargetConfig) string {
	base := source.WorkBase
	if base == "" {
		base = source.OutBase
	}
	return filepath.Join(base, ".pekit", "dependencies", string(target.Kind)+"."+target.Name+".json")
}

func renderDependencyPayload(version Version, target TargetConfig, provider string) (DependencyPayload, error) {
	all := map[string]map[string]string{}
	for _, providerName := range sortedKeys(target.Dependencies) {
		renderedProvider := map[string]string{}
		renderedNames := map[string]string{}
		for _, depName := range sortedKeys(target.Dependencies[providerName]) {
			renderedName, err := RenderTemplate(depName, TemplateContext{Version: version})
			if err != nil {
				return DependencyPayload{}, wrapDiag("template", "render dependency name "+depName, err)
			}
			if err := pack.ValidateCapabilityName(renderedName); err != nil {
				return DependencyPayload{}, diag("invalid_dependency", "rendered dependency name %q is not valid: %v", renderedName, err)
			}
			renderedConstraint, err := RenderTemplate(target.Dependencies[providerName][depName], TemplateContext{Version: version})
			if err != nil {
				return DependencyPayload{}, wrapDiag("template", "render dependency constraint "+depName, err)
			}
			if providerName == "peipkg" {
				if err := validateConstraintString(renderedConstraint); err != nil {
					return DependencyPayload{}, err
				}
			}
			if prev, exists := renderedNames[renderedName]; exists {
				return DependencyPayload{}, diag("dependency_collision", "dependencies %q and %q both render to %q for provider %q", prev, depName, renderedName, providerName)
			}
			renderedNames[renderedName] = depName
			renderedProvider[renderedName] = renderedConstraint
		}
		all[providerName] = renderedProvider
	}
	if provider != "" && len(all) > 0 {
		if _, ok := all[provider]; !ok {
			return DependencyPayload{}, diag("missing_dependency_provider", "env selected dependency provider %q but %s.%s defines only: %s", provider, target.Kind, target.Name, strings.Join(sortedKeys(all), ", "))
		}
	}
	selected := map[string]string{}
	if provider != "" {
		if deps, ok := all[provider]; ok {
			selected = deps
		}
	}
	payload := DependencyPayload{
		Command:      string(target.Kind),
		Target:       target.Name,
		Provider:     provider,
		Version:      version.Raw,
		Dependencies: selected,
		AllProviders: all,
	}
	if payload.Dependencies == nil {
		payload.Dependencies = map[string]string{}
	}
	if payload.AllProviders == nil {
		payload.AllProviders = map[string]map[string]string{}
	}
	return payload, nil
}

func dependencyList(deps map[string]string) string {
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	for _, name := range sortedKeys(deps) {
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(deps[name])
		b.WriteByte('\n')
	}
	return b.String()
}
