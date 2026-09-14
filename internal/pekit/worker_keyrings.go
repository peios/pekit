package pekit

import "fmt"

// Legacy string leaves remain usable by the coordinator, but are never
// implicitly exported. Worker access is granted by the operator's keyring,
// then narrowed by the recipe's per-target request.
func resolveKeyringAccess(inv Invocation, root string, workspace *WorkspaceConfig) (map[string]string, error) {
	out := map[string]string{}
	overlay(out, inv.ResolvedKeyringAccess)
	roots := []string{root}
	if workspace != nil {
		roots = append([]string{workspace.Root}, roots...)
	}
	for _, item := range inv.Keyrings {
		path, err := resolveKeyringPath(item, inv.Cwd, roots)
		if err != nil {
			return nil, err
		}
		raw, err := loadTOML(path)
		if err != nil {
			return nil, err
		}
		// Validate with the same parser used by coordinator signing.
		if err := flattenKeyring(map[string]string{}, "", raw); err != nil {
			return nil, err
		}
		var walk func(string, map[string]any)
		walk = func(prefix string, table map[string]any) {
			for key, v := range table {
				name := key
				if prefix != "" {
					name = prefix + "." + key
				}
				access := "signing"
				if sub, ok := v.(map[string]any); ok {
					if _, typed := sub["value"]; !typed {
						walk(name, sub)
						continue
					}
					access = sub["access"].(string)
				}
				out[envNameFromKeyring(name)] = access
			}
		}
		walk("", raw)
	}
	// An untyped CLI override must not inherit an earlier public grant.
	for key := range inv.KeyringValues {
		out[envNameFromKeyring(key)] = "signing"
	}
	return out, nil
}

func workerKeyrings(inv Invocation, root string, workspace *WorkspaceConfig, target TargetConfig, values map[string]string) (map[string]string, error) {
	access, err := resolveKeyringAccess(inv, root, workspace)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, entry := range target.KeyringInputs {
		name := envNameFromKeyring(entry)
		value, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("keyring input %s is not configured", entry)
		}
		switch access[name] {
		case "public":
		case "acquisition":
			if target.Kind != CommandBuild || target.Name != "vendor" {
				return nil, fmt.Errorf("acquisition keyring input %s is restricted to build.vendor", entry)
			}
		default:
			return nil, fmt.Errorf("keyring input %s is coordinator-only; worker inputs require an explicit public or acquisition access grant in the keyring", entry)
		}
		out[name] = value
	}
	return out, nil
}
