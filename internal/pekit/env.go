package pekit

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type CommandEnv struct {
	Values map[string]string
	Outer  map[string]string
	Script string
	Wrap   ShellCommand
}

func BuildCommandEnv(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, target TargetConfig, targetOut string) (CommandEnv, error) {
	values := map[string]string{}
	var userEnv []EnvVar
	if workspace != nil {
		appendEnvLayer(&userEnv, values, workspace.Env)
	}
	sourceEnvFile := EnvFile{}
	sourceDelegated := source.Kind != "recipe" && source.SourceRoot != "" && source.SourceRoot != recipe.Root
	if sourceDelegated && recipe.Delegate.AllowsEnv() {
		if sourceRecipe, err := LoadRecipe(filepath.Join(source.SourceRoot, "pekit.toml")); err == nil {
			appendEnvLayer(&userEnv, values, sourceRecipe.Env)
		}
		var err error
		sourceEnvFile, err = selectedEnvFile(ctx.Inv, source.SourceRoot, true)
		if err != nil {
			return CommandEnv{}, err
		}
		appendEnvLayer(&userEnv, values, sourceEnvFile.Env)
	}
	appendEnvLayer(&userEnv, values, recipe.Env)
	envFile, err := selectedEnvFile(ctx.Inv, recipe.Root, false)
	if err != nil {
		return CommandEnv{}, err
	}
	if envFile.Path != "" {
		appendEnvLayer(&userEnv, values, envFile.Env)
	}
	dependencyProvider := ""
	if sourceDelegated && recipe.Delegate.AllowsEnv() && sourceEnvFile.DependencyProvider != "" {
		dependencyProvider = sourceEnvFile.DependencyProvider
	}
	if envFile.DependencyProvider != "" {
		dependencyProvider = envFile.DependencyProvider
	}
	for _, env := range userEnv {
		if strings.HasPrefix(env.Name, "PEKIT_") {
			return CommandEnv{}, diag("reserved_env", "user env cannot set reserved variable %s", env.Name)
		}
	}
	keyringEnv, err := resolveKeyrings(ctx.Inv, recipe.Root, workspace)
	if err != nil {
		return CommandEnv{}, err
	}
	for key := range keyringEnv {
		if _, exists := values[key]; exists {
			return CommandEnv{}, diag("env_collision", "keyring export %s collides with normal env", key)
		}
	}
	managed, err := managedEnv(ctx, recipe, workspace, source, version, target, targetOut, dependencyProvider)
	if err != nil {
		return CommandEnv{}, err
	}
	all := map[string]string{}
	overlay(all, managed)
	for key := range keyringEnv {
		if _, exists := managed[key]; exists {
			return CommandEnv{}, diag("env_collision", "keyring export %s collides with managed env", key)
		}
	}
	overlay(all, keyringEnv)
	overlay(all, values)
	wrap := ShellCommand{}
	if workspace != nil && !workspace.Wrap.Empty() {
		wrap = workspace.Wrap
	}
	if sourceDelegated && recipe.Delegate.AllowsWrap() && !sourceEnvFile.Wrap.Empty() {
		wrap = sourceEnvFile.Wrap
	}
	if recipe.Wrap.Empty() == false {
		wrap = recipe.Wrap
	}
	if envFile.Wrap.Empty() == false {
		wrap = envFile.Wrap
	}
	if ctx.Inv.Verbose {
		ctx.Renderer.Event(Event{
			Type:    "env",
			Target:  target.Name,
			Version: version.Raw,
			Message: "managed=" + strings.Join(sortedKeys(managed), ",") + " keyring=" + strings.Join(sortedKeys(keyringEnv), ",") + " env=" + strings.Join(sortedKeys(values), ","),
		})
	}
	return CommandEnv{Values: all, Outer: managed, Script: exportScriptLayers(managed, keyringEnv, userEnv), Wrap: wrap}, nil
}

func managedEnv(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, target TargetConfig, targetOut, dependencyProvider string) (map[string]string, error) {
	values := map[string]string{
		"PEKIT_RECIPE_ROOT":      recipe.Root,
		"PEKIT_SOURCE_ROOT":      source.SourceRoot,
		"PEKIT_LITERAL_ROOT":     source.LiteralRoot,
		"PEKIT_ROOT":             source.WorkBase,
		"PEKIT_OUT_BASE":         source.OutBase,
		"PEKIT_OUT":              targetOut,
		"PEKIT_COMMAND":          string(target.Kind),
		"PEKIT_TARGET":           target.Name,
		"PEKIT_BUILD_TIMESTAMP":  strconv.FormatInt(ctx.Start.Unix(), 10),
		"PEKIT_SOURCE_TIMESTAMP": strconv.FormatInt(source.Timestamp, 10),
	}
	if workspace != nil {
		values["PEKIT_WORKSPACE_ROOT"] = workspace.Root
	} else {
		values["PEKIT_WORKSPACE_ROOT"] = ""
	}
	if version.Raw != "" {
		values["PEKIT_VERSION"] = version.Raw
		values["PEKIT_VERSION_MAJOR"] = version.Major
		values["PEKIT_VERSION_MINOR"] = version.Minor
		values["PEKIT_VERSION_PATCH"] = version.Patch
		values["PEKIT_VERSION_PRERELEASE"] = version.Prerelease
		values["PEKIT_VERSION_BUILDMETA"] = version.BuildMeta
	}
	seenDeps := map[string]string{}
	for _, dep := range target.Needs {
		name := "PEKIT_" + targetEnvName(dep) + "_OUT"
		if prev, exists := seenDeps[name]; exists && prev != dep {
			return nil, diag("env_collision", "direct dependency outputs %s and %s both export %s", prev, dep, name)
		}
		seenDeps[name] = dep
		values[name] = targetStage(source, CommandBuild, dep)
	}
	dependencyEnv, err := dependencyManagedEnv(source, version, target, dependencyProvider)
	if err != nil {
		return nil, err
	}
	overlay(values, dependencyEnv)
	return values, nil
}

func targetEnvName(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

func selectedEnvFile(inv Invocation, recipeRoot string, missingOKOverride bool) (EnvFile, error) {
	name := inv.EnvName
	if name == "" {
		name = "main"
	}
	if name == "none" {
		return EnvFile{}, nil
	}
	file := "env.pekit.toml"
	missingOK := true
	if name != "main" {
		file = name + ".env.pekit.toml"
		missingOK = false
	}
	if missingOKOverride {
		missingOK = true
	}
	return LoadEnvFile(filepath.Join(recipeRoot, file), missingOK)
}

func resolveKeyrings(inv Invocation, recipeRoot string, workspace *WorkspaceConfig) (map[string]string, error) {
	out := map[string]string{}
	overlay(out, inv.ResolvedKeyringEnv)
	searchRoots := []string{recipeRoot}
	if workspace != nil {
		searchRoots = append([]string{workspace.Root}, searchRoots...)
	}
	for _, item := range inv.Keyrings {
		path, err := resolveKeyringPath(item, inv.Cwd, searchRoots)
		if err != nil {
			return nil, err
		}
		values, err := loadKeyring(path)
		if err != nil {
			return nil, err
		}
		overlay(out, values)
	}
	for key, value := range inv.KeyringValues {
		out[envNameFromKeyring(key)] = value
	}
	return out, nil
}

func resolveKeyringPath(value, cwd string, roots []string) (string, error) {
	if pathLike(value) {
		path, err := absPath(cwd, value)
		if err != nil {
			return "", wrapDiag("invalid_path", "resolve keyring", err)
		}
		if !fileExists(path) {
			return "", diagAt("missing_keyring", path, "keyring file does not exist")
		}
		return path, nil
	}
	name := value + ".keyring.pekit.toml"
	for _, root := range roots {
		path := filepath.Join(root, name)
		if fileExists(path) {
			return path, nil
		}
	}
	path, err := absPath(cwd, value)
	if err != nil {
		return "", wrapDiag("invalid_path", "resolve keyring", err)
	}
	if !fileExists(path) {
		return "", diagAt("missing_keyring", path, "keyring file does not exist")
	}
	return path, nil
}

func pathLike(value string) bool {
	return filepath.IsAbs(value) || strings.HasPrefix(value, ".") || strings.HasPrefix(value, "~") || strings.Contains(value, "/")
}

func loadKeyring(path string) (map[string]string, error) {
	raw, err := loadTOML(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := flattenKeyring(out, "", raw); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenKeyring(out map[string]string, prefix string, raw map[string]any) error {
	for key, value := range raw {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch v := value.(type) {
		case string:
			out[envNameFromKeyring(path)] = v
		case map[string]any:
			if _, ok := v["path"]; ok {
				return diag("unsupported_keyring_entry", "typed keyring entry %s is not supported yet", path)
			}
			if _, ok := v["content"]; ok {
				return diag("unsupported_keyring_entry", "typed keyring entry %s is not supported yet", path)
			}
			if err := flattenKeyring(out, path, v); err != nil {
				return err
			}
		default:
			return fmt.Errorf("keyring %s must be a string leaf", path)
		}
	}
	return nil
}

func overlay(dst map[string]string, src map[string]string) {
	for key, value := range src {
		dst[key] = value
	}
}

func appendEnvLayer(out *[]EnvVar, final map[string]string, vars []EnvVar) {
	for _, env := range vars {
		*out = append(*out, env)
		final[env.Name] = env.Value
	}
}

func envSlice(values map[string]string) []string {
	env := os.Environ()
	seen := map[string]bool{}
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		seen[key] = true
	}
	for key, value := range values {
		entry := key + "=" + value
		if seen[key] {
			for idx, item := range env {
				if strings.HasPrefix(item, key+"=") {
					env[idx] = entry
					break
				}
			}
		} else {
			env = append(env, entry)
		}
	}
	return env
}
