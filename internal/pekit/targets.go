package pekit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func mergeDelegatedRecipe(base RecipeConfig, source SourceState) (RecipeConfig, error) {
	if !base.Delegate.AllowsBuild() || source.SourceRoot == "" || source.SourceRoot == base.Root {
		return base, nil
	}
	path := filepath.Join(source.SourceRoot, "pekit.toml")
	if !fileExists(path) {
		return base, nil
	}
	delegated, err := LoadRecipe(path)
	if err != nil {
		return RecipeConfig{}, err
	}
	out := base
	for kind, targets := range delegated.Targets {
		if out.Targets[kind] == nil {
			out.Targets[kind] = map[string]TargetConfig{}
		}
		for name, target := range targets {
			target.Owner = "source"
			if _, exists := out.Targets[kind][name]; !exists {
				out.Targets[kind][name] = target
			}
		}
	}
	return out, nil
}

func executeSelectedTargets(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version, kind Command, selectors []string, member string) error {
	targets := recipe.Targets[kind]
	selected, err := selectTargets(kind, targets, selectors)
	if err != nil {
		return err
	}
	builds, err := buildOrderForTargets(ctx.Inv, source, recipe.Targets[CommandBuild], selected, kind, targets)
	if err != nil {
		return err
	}
	for _, build := range builds {
		if err := runTarget(ctx, recipe, workspace, source, version, build, member); err != nil {
			return err
		}
	}
	if kind == CommandBuild {
		return nil
	}
	for _, target := range selected {
		if err := runTarget(ctx, recipe, workspace, source, version, target, member); err != nil {
			return err
		}
	}
	return nil
}

func selectTargets(kind Command, targets map[string]TargetConfig, selectors []string) ([]TargetConfig, error) {
	if len(targets) == 0 {
		return nil, diag("missing_target", "recipe has no %s targets", kind)
	}
	if len(selectors) == 0 {
		if t, ok := targets["main"]; ok {
			return []TargetConfig{t}, nil
		}
		if kind == CommandBuild && len(targets) == 1 {
			for _, t := range targets {
				return []TargetConfig{t}, nil
			}
		}
		return nil, diag("ambiguous_target", "no %s.main target; available targets: %s", kind, strings.Join(sortedKeys(targets), ", "))
	}
	out := make([]TargetConfig, 0, len(selectors))
	for _, name := range selectors {
		if err := validateSelector("target", name); err != nil {
			return nil, err
		}
		t, ok := targets[name]
		if !ok {
			return nil, diag("missing_target", "unknown %s target %q; available targets: %s", kind, name, strings.Join(sortedKeys(targets), ", "))
		}
		out = append(out, t)
	}
	return out, nil
}

func buildOrderForTargets(inv Invocation, source SourceState, buildTargets map[string]TargetConfig, selected []TargetConfig, kind Command, commandTargets map[string]TargetConfig) ([]TargetConfig, error) {
	if kind == CommandBuild {
		return topoBuildsForInvocation(inv, source, buildTargets, selected)
	}
	needed := make([]TargetConfig, 0)
	for _, t := range selected {
		for _, dep := range t.Needs {
			bt, ok := buildTargets[dep]
			if !ok {
				return nil, diag("missing_target", "%s.%s needs missing build target %q", kind, t.Name, dep)
			}
			needed = append(needed, bt)
		}
	}
	return topoBuildsForInvocation(inv, source, buildTargets, needed)
}

func topoBuilds(all map[string]TargetConfig, roots []TargetConfig) ([]TargetConfig, error) {
	return topoBuildsForInvocation(Invocation{}, SourceState{}, all, roots)
}

func topoBuildsForInvocation(inv Invocation, source SourceState, all map[string]TargetConfig, roots []TargetConfig) ([]TargetConfig, error) {
	visited := map[string]bool{}
	visiting := map[string]bool{}
	var out []TargetConfig
	var visit func(TargetConfig, []string) error
	visit = func(t TargetConfig, stack []string) error {
		if visited[t.Name] {
			return nil
		}
		if visiting[t.Name] {
			return diag("target_cycle", "build dependency cycle: %s -> %s", strings.Join(stack, " -> "), t.Name)
		}
		if source.WorkBase != "" && wantsReuseBuild(inv, t) {
			visited[t.Name] = true
			out = append(out, t)
			return nil
		}
		visiting[t.Name] = true
		for _, dep := range t.Needs {
			next, ok := all[dep]
			if !ok {
				return diag("missing_target", "build.%s needs missing build target %q", t.Name, dep)
			}
			if err := visit(next, append(stack, t.Name)); err != nil {
				return err
			}
		}
		visiting[t.Name] = false
		visited[t.Name] = true
		out = append(out, t)
		return nil
	}
	for _, root := range roots {
		if err := visit(root, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func cleanRecipe(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, member string) error {
	selectors := ctx.Inv.selectors()
	runCleanTarget := !ctx.Inv.OutputOnly
	removeOutput := !ctx.Inv.TargetOnly
	if runCleanTarget {
		targets := recipe.Targets[CommandClean]
		var selected []TargetConfig
		if len(selectors) == 0 {
			if t, ok := targets["main"]; ok {
				selected = []TargetConfig{t}
			} else if ctx.Inv.TargetOnly {
				return diag("missing_target", "--target-only requires clean.main or an explicit clean target")
			}
		} else {
			t, ok := targets[selectors[0]]
			if !ok {
				return diag("missing_target", "unknown clean target %q", selectors[0])
			}
			selected = []TargetConfig{t}
		}
		for _, target := range selected {
			if err := runTarget(ctx, recipe, workspace, source, Version{}, target, member); err != nil {
				return err
			}
		}
	}
	if removeOutput {
		if ctx.Inv.DryRun {
			ctx.Renderer.Event(Event{Type: "clean_plan", Member: member, Path: source.OutBase, Message: "would remove managed output"})
			return nil
		}
		if err := os.RemoveAll(source.OutBase); err != nil {
			return wrapDiag("clean_output", source.OutBase, err)
		}
		ctx.Renderer.Event(Event{Type: "clean", Member: member, Path: source.OutBase, Message: "removed managed output"})
	}
	return nil
}

func validateNoBuildTargets(inv Invocation, builds map[string]TargetConfig) error {
	if inv.NoBuild == nil || *inv.NoBuild == "" {
		return nil
	}
	for _, item := range strings.Split(*inv.NoBuild, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		if _, ok := builds[name]; !ok {
			return diag("missing_target", "--no-build names missing build target %q", name)
		}
	}
	return nil
}

func requireBuildTarget(builds map[string]TargetConfig, name string) (TargetConfig, error) {
	if name == "" {
		name = "main"
	}
	t, ok := builds[name]
	if !ok {
		return TargetConfig{}, fmt.Errorf("missing build target %q", name)
	}
	return t, nil
}
