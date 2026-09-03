package pekit

import (
	"fmt"
	"strings"
	"sync"
)

type WorkspaceMember struct {
	ID         string
	Root       string
	RecipePath string
}

type memberResult struct {
	Member  WorkspaceMember
	Err     error
	Skipped bool
}

func runWorkspace(ctx *Context) error {
	ws, err := locateWorkspace(ctx.Inv)
	if err != nil {
		return err
	}
	members, err := discoverWorkspaceMembers(ws)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return diag("empty_workspace", "workspace has no members")
	}
	ctx.Renderer.Event(Event{Type: "workspace", Command: string(ctx.Inv.DelegateCommand), Path: ws.Path, Message: fmt.Sprintf("loaded workspace with %d members", len(members))})
	resolvedKeyrings, err := resolveKeyrings(ctx.Inv, ws.Root, &ws)
	if err != nil {
		return err
	}
	workspaceInv := ctx.Inv
	workspaceInv.Keyrings = nil
	workspaceInv.KeyringValues = map[string]string{}
	workspaceInv.ResolvedKeyringEnv = resolvedKeyrings
	localCtx := *ctx
	localCtx.Inv = workspaceInv
	ctx = &localCtx
	if ctx.Inv.DryRun {
		for _, member := range members {
			ctx.Renderer.Event(Event{Type: "workspace_member", Member: member.ID, Path: member.RecipePath, Message: "selected member"})
		}
	}
	selectorPlan, err := planWorkspaceSelectors(ctx, ws, members)
	if err != nil {
		return err
	}
	if ctx.Inv.DelegateCommand == CommandPublish {
		if err := preflightWorkspacePublishDestinations(ctx, ws, members, selectorPlan); err != nil {
			return err
		}
	}
	results := runWorkspaceMembers(ctx, ws, members, selectorPlan)
	failed := 0
	succeeded := 0
	skipped := 0
	for _, result := range results {
		if result.Skipped {
			skipped++
		} else if result.Err != nil {
			failed++
		} else {
			succeeded++
		}
	}
	ctx.Renderer.Event(Event{Type: "workspace_summary", Message: fmt.Sprintf("%d succeeded, %d failed, %d skipped", succeeded, failed, skipped)})
	if failed > 0 {
		return diag("workspace_failed", "%d workspace member(s) failed", failed)
	}
	return nil
}

func preflightWorkspacePublishDestinations(ctx *Context, ws WorkspaceConfig, members []WorkspaceMember, selectorPlan workspaceSelectorPlan) error {
	for _, member := range members {
		if reason := selectorPlan.Skip[member.ID]; reason != "" {
			continue
		}
		memberInv := ctx.Inv
		memberInv.Command = CommandPublish
		memberInv.WorkspaceMode = false
		memberInv.WorkspaceFlag = ws.Path
		memberInv.SuppressUnsupportedVersion = memberInv.AllowUnused
		memberInv.DryRun = true
		memberInv.RefreshSource = false
		if selectors, ok := selectorPlan.Selectors[member.ID]; ok {
			memberInv.Positionals = append([]string(nil), selectors...)
			memberInv.AfterDoubleDash = nil
		}
		memberCtx := *ctx
		memberCtx.Inv = memberInv
		if err := preflightMemberPublishDestinations(&memberCtx, ws, member); err != nil {
			return err
		}
	}
	return nil
}

func preflightMemberPublishDestinations(ctx *Context, ws WorkspaceConfig, member WorkspaceMember) error {
	recipe, err := LoadRecipe(member.RecipePath)
	if err != nil {
		return nil
	}
	versions, err := ResolveVersions(ctx.Inv, recipe.Source)
	if err != nil {
		return nil
	}
	for _, version := range versions {
		source, err := ResolveSource(ctx, recipe, version)
		if err != nil {
			continue
		}
		effectiveRecipe, err := mergeDelegatedRecipe(recipe, source)
		if err != nil {
			continue
		}
		ops, err := planKnownPublishOps(ctx.Inv, effectiveRecipe, &ws, source, version)
		if err != nil {
			continue
		}
		if err := reservePublishDestinations(ctx, ops, member.ID); err != nil {
			return err
		}
	}
	return nil
}

func planKnownPublishOps(inv Invocation, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, version Version) (publishPlan, error) {
	packages, err := loadEffectivePackages(recipe, workspace, source)
	if err != nil {
		if diagCode(err) == "missing_package" && recipe.Delegate.AllowsPackages() && source.SourceRoot != recipe.Root && !dirExists(source.SourceRoot) {
			return publishPlan{}, nil
		}
		return publishPlan{}, err
	}
	selected, err := selectPackages(inv, packages)
	if err != nil {
		return publishPlan{}, err
	}
	var instances []PackageInstance
	for _, def := range selected {
		expanded, err := expandPackageInstances(def, source, recipe, workspace, version)
		if err != nil {
			return publishPlan{}, nil
		}
		instances = append(instances, expanded...)
	}
	if len(instances) == 0 {
		return publishPlan{}, nil
	}
	if err := validateEmittedPackageNames(instances); err != nil {
		return publishPlan{}, err
	}
	return planPublishOps(workspace, recipe, instances)
}

type workspaceSelectorPlan struct {
	Selectors map[string][]string
	Skip      map[string]string
}

func runWorkspaceMembers(ctx *Context, ws WorkspaceConfig, members []WorkspaceMember, selectorPlan workspaceSelectorPlan) []memberResult {
	jobs := ctx.Inv.Jobs
	if jobs < 1 {
		jobs = 1
	}
	results := make([]memberResult, len(members))
	work := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	stop := false
	worker := func() {
		defer wg.Done()
		for idx := range work {
			mu.Lock()
			if stop {
				results[idx] = memberResult{Member: members[idx], Skipped: true}
				mu.Unlock()
				continue
			}
			mu.Unlock()
			member := members[idx]
			if reason := selectorPlan.Skip[member.ID]; reason != "" {
				ctx.Renderer.Event(Event{Type: "member_skipped", Member: member.ID, Message: reason})
				results[idx] = memberResult{Member: member, Skipped: true}
				continue
			}
			memberCtx := *ctx
			memberInv := ctx.Inv
			memberInv.Command = memberInv.DelegateCommand
			memberInv.WorkspaceMode = false
			memberInv.WorkspaceFlag = ws.Path
			memberInv.SuppressUnsupportedVersion = memberInv.AllowUnused
			if selectors, ok := selectorPlan.Selectors[member.ID]; ok {
				memberInv.Positionals = append([]string(nil), selectors...)
				memberInv.AfterDoubleDash = nil
			}
			memberCtx.Inv = memberInv
			err := runRecipe(&memberCtx, member.ID, member.RecipePath)
			results[idx] = memberResult{Member: member, Err: err}
			if err != nil {
				ctx.Renderer.Error(Diagnostic{Code: "member_failed", Member: member.ID, Message: err.Error()})
				if ctx.Inv.FailFast {
					mu.Lock()
					stop = true
					mu.Unlock()
				}
			}
		}
	}
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go worker()
	}
	for idx := range members {
		mu.Lock()
		shouldStop := stop
		mu.Unlock()
		if shouldStop {
			results[idx] = memberResult{Member: members[idx], Skipped: true}
			continue
		}
		work <- idx
	}
	close(work)
	wg.Wait()
	return results
}

func planWorkspaceSelectors(ctx *Context, ws WorkspaceConfig, members []WorkspaceMember) (workspaceSelectorPlan, error) {
	plan := workspaceSelectorPlan{Selectors: map[string][]string{}, Skip: map[string]string{}}
	selectors := ctx.Inv.selectors()
	if !ctx.Inv.AllowUnused || len(selectors) == 0 || ctx.Inv.All {
		return plan, nil
	}
	cmd := ctx.Inv.DelegateCommand
	if cmd != CommandBuild && cmd != CommandTest && cmd != CommandInstall && cmd != CommandClean && cmd != CommandPackage && cmd != CommandPublish {
		return plan, nil
	}
	availableByMember := map[string]map[string]bool{}
	seen := map[string]bool{}
	for _, member := range members {
		available, err := workspaceMemberSelectors(ws, member, cmd)
		if err != nil {
			return plan, err
		}
		availableByMember[member.ID] = available
		for selector := range available {
			seen[selector] = true
		}
	}
	for _, selector := range selectors {
		key := workspaceSelectorKey(cmd, selector)
		if !seen[key] {
			return plan, diag("missing_selector", "workspace selector %q matches no selected member", selector)
		}
	}
	for _, member := range members {
		available := availableByMember[member.ID]
		var kept []string
		var suppressed []string
		for _, selector := range selectors {
			key := workspaceSelectorKey(cmd, selector)
			if available[key] {
				kept = append(kept, selector)
			} else {
				suppressed = append(suppressed, selector)
			}
		}
		if len(suppressed) > 0 {
			ctx.Renderer.Event(Event{Type: "workspace_selector_suppressed", Member: member.ID, Message: "suppressed selector(s): " + strings.Join(suppressed, ", ")})
		}
		if len(kept) == 0 {
			plan.Skip[member.ID] = "no requested selectors apply to member"
			continue
		}
		plan.Selectors[member.ID] = kept
	}
	return plan, nil
}

func workspaceMemberSelectors(ws WorkspaceConfig, member WorkspaceMember, cmd Command) (map[string]bool, error) {
	recipe, err := LoadRecipe(member.RecipePath)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	switch cmd {
	case CommandBuild, CommandTest, CommandInstall, CommandClean:
		for name := range recipe.Targets[cmd] {
			out[name] = true
		}
	case CommandPackage, CommandPublish:
		pkgs, err := loadEffectivePackages(recipe, &ws, cleanSourceState(recipe))
		if err != nil {
			return nil, err
		}
		for _, pkg := range pkgs {
			out[pkg.Selector] = true
		}
	}
	return out, nil
}

func workspaceSelectorKey(cmd Command, selector string) string {
	if cmd == CommandPackage || cmd == CommandPublish {
		def, _, _ := strings.Cut(selector, ":")
		return def
	}
	return selector
}
