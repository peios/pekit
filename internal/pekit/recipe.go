package pekit

import (
	"path/filepath"
	"strings"
)

func runRecipe(ctx *Context, member, recipePathOverride string) error {
	loc, inv, err := recipeLocationForRun(ctx.Inv, recipePathOverride)
	if err != nil {
		return err
	}
	localCtx := *ctx
	localCtx.Inv = inv
	ctx = &localCtx
	recipe, err := LoadRecipe(loc.Path)
	if err != nil {
		return err
	}
	workspace := loc.Workspace
	if recipePathOverride != "" && ctx.Inv.WorkspaceFlag != "" {
		ws, err := locateWorkspace(ctx.Inv)
		if err == nil {
			workspace = &ws
		}
	}
	// A delegate recipe borrows its build targets from the fetched source,
	// which is not resolved yet — its names are validated per-version after
	// mergeDelegatedRecipe instead of against the thin delegate stub here.
	if !recipe.Delegate.AllowsBuild() {
		if err := validateNoBuildTargets(ctx.Inv, recipe.Targets[CommandBuild]); err != nil {
			return err
		}
	}
	ctx.Renderer.Event(Event{Type: "recipe", Member: member, Command: string(ctx.Inv.EffectiveCommand()), Path: loc.Path, Message: "loaded recipe"})
	if ctx.Inv.Verbose {
		ctx.Renderer.Event(Event{Type: "recipe_detail", Member: member, Path: recipe.Root, Message: "out_dir=" + recipe.OutDir + " targets=" + recipeTargetSummary(recipe)})
	}
	cmd := ctx.Inv.EffectiveCommand()
	if cmd == CommandClean {
		source := cleanSourceState(recipe)
		recipe, err = mergeDelegatedRecipe(recipe, source)
		if err != nil {
			return err
		}
		return cleanRecipe(ctx, recipe, workspace, source, member)
	}
	// gen and verify operate on the committed tree — no version selection and no
	// source clone — so they short-circuit before source resolution.
	if cmd == CommandGen {
		return runGen(ctx, recipe, workspace, cleanSourceState(recipe), ctx.Inv.selectors(), member)
	}
	if cmd == CommandVerify {
		return runVerify(ctx, recipe, workspace, cleanSourceState(recipe), ctx.Inv.selectors(), member)
	}
	// lock resolves and pins sources without running targets, so it skips the
	// gen drift gates.
	if cmd == CommandLock {
		return runLockCmd(ctx, recipe, member)
	}
	// Consuming commands run the scoped drift gates first, so no build/test/
	// package/publish ever proceeds from a stale generated tree (unless
	// --no-verify is passed).
	if err := preflightVerify(ctx, recipe, workspace, member); err != nil {
		return err
	}
	versions, err := ResolveVersions(ctx.Inv, recipe.Source)
	if err != nil {
		return err
	}
	if (cmd == CommandTest || cmd == CommandInstall) && len(versions) > 1 {
		return diag("invalid_version_selector", "%s requires a single resolved version", cmd)
	}
	for _, version := range versions {
		source, err := ResolveSource(ctx, recipe, version)
		if err != nil {
			return err
		}
		if ctx.Inv.Verbose {
			ctx.Renderer.Event(Event{Type: "source", Member: member, Version: version.Raw, Path: source.SourceRoot, Message: source.Kind + " " + source.ProvenanceRef})
		}
		effectiveRecipe, err := mergeDelegatedRecipe(recipe, source)
		if err != nil {
			return err
		}
		if recipe.Delegate.AllowsBuild() {
			if err := validateNoBuildTargets(ctx.Inv, effectiveRecipe.Targets[CommandBuild]); err != nil {
				return err
			}
		}
		switch cmd {
		case CommandBuild, CommandTest, CommandInstall:
			if err := executeSelectedTargets(ctx, effectiveRecipe, workspace, source, version, cmd, ctx.Inv.selectors(), member); err != nil {
				return err
			}
		case CommandPackage:
			if err := packageOrPublish(ctx, effectiveRecipe, workspace, source, version, false, member); err != nil {
				return err
			}
		case CommandPublish:
			if err := packageOrPublish(ctx, effectiveRecipe, workspace, source, version, true, member); err != nil {
				return err
			}
		default:
			return diag("unknown_command", "unsupported command %q", cmd)
		}
	}
	return nil
}

func recipeTargetSummary(recipe RecipeConfig) string {
	parts := make([]string, 0, len(recipe.Targets))
	for _, kind := range []Command{CommandBuild, CommandTest, CommandInstall, CommandClean, CommandGen} {
		targets := recipe.Targets[kind]
		if len(targets) == 0 {
			continue
		}
		parts = append(parts, string(kind)+":"+strings.Join(sortedKeys(targets), ","))
	}
	return strings.Join(parts, " ")
}

func recipeLocationForRun(inv Invocation, recipePathOverride string) (RecipeLocation, Invocation, error) {
	if recipePathOverride == "" {
		return locateRecipe(inv)
	}
	path, err := filepath.Abs(recipePathOverride)
	if err != nil {
		return RecipeLocation{}, inv, err
	}
	return RecipeLocation{Root: filepath.Dir(path), Path: path}, inv, nil
}

func cleanSourceState(recipe RecipeConfig) SourceState {
	outBase := recipe.OutDir
	if !filepath.IsAbs(outBase) {
		outBase = filepath.Join(recipe.Root, outBase)
	}
	return SourceState{
		Kind:          "recipe",
		Scope:         "recipe",
		OutBase:       outBase,
		WorkBase:      outBase,
		SourceRoot:    recipe.Root,
		LiteralRoot:   recipe.Root,
		ProvenanceRef: "recipe:" + recipe.Root,
		Timestamp:     gitWorktreeTimestamp(recipe.Root),
	}
}
