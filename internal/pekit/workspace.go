package pekit

import (
	"fmt"
	"strings"
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
	members, err = filterMembersByTag(ctx.Inv, members)
	if err != nil {
		return err
	}
	ctx.Renderer.Event(Event{Type: "workspace", Command: string(ctx.Inv.DelegateCommand), Path: ws.Path, Message: fmt.Sprintf("loaded workspace with %d members", len(members))})
	resolvedKeyrings, err := resolveKeyrings(ctx.Inv, ws.Root, &ws)
	if err != nil {
		return err
	}
	access, err := resolveKeyringAccess(ctx.Inv, ws.Root, &ws)
	if err != nil {
		return err
	}
	workspaceInv := ctx.Inv
	workspaceInv.Keyrings = nil
	workspaceInv.KeyringValues = map[string]string{}
	workspaceInv.ResolvedKeyringEnv = resolvedKeyrings
	workspaceInv.ResolvedKeyringAccess = access
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
	journal, err := openWorkspaceJournal(ctx)
	if err != nil {
		return err
	}
	defer journal.close()
	if journal != nil {
		for _, member := range members {
			if journal.done[member.ID] && selectorPlan.Skip[member.ID] == "" {
				selectorPlan.Skip[member.ID] = "already finished in journal " + journal.path
			}
		}
	}
	if ctx.Inv.DelegateCommand == CommandPublish {
		if err := preflightWorkspacePublishDestinations(ctx, ws, members, selectorPlan); err != nil {
			return err
		}
	}
	waits, err := planWorkspaceOrder(ctx, ws, members, selectorPlan)
	if err != nil {
		return err
	}
	if ctx.Inv.DryRun || ctx.Inv.Verbose {
		for _, member := range members {
			if _, ordered := waits[member.ID]; ordered {
				ctx.Renderer.Event(Event{Type: "workspace_order", Member: member.ID, Message: describeWaits(waits[member.ID])})
			}
		}
	}
	results, stopped := runWorkspaceMembers(ctx, ws, members, selectorPlan, waits, journal)
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
	if stopped {
		return diag("workspace_stopped", "stopped on request; repeat the command with --journal %s to resume", journal.path)
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
	versions, err := resolveRecipeVersions(ctx, recipe)
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

// runWorkspaceMembers runs up to --jobs members at once. A member starts once
// every member it waits for has finished (see planWorkspaceOrder); among
// runnable members, lower ids start first. When nothing is runnable and
// nothing is running, the remaining members wait on each other in a cycle and
// breakCycle chooses one to start anyway. A member runs even if one it waited
// for failed: it then resolves that dependency from what the provider already
// has, as an unordered run would.
//
// With a journal, each member that succeeds is recorded as it finishes, and a
// stop request is honoured between members: running members finish, nothing
// new starts, and stopped reports it.
func runWorkspaceMembers(ctx *Context, ws WorkspaceConfig, members []WorkspaceMember, selectorPlan workspaceSelectorPlan, waits map[string][]string, journal *workspaceJournal) (results []memberResult, stopped bool) {
	jobs := ctx.Inv.Jobs
	if jobs < 1 {
		jobs = 1
	}
	results = make([]memberResult, len(members))
	started := make([]bool, len(members))
	pos := map[string]int{}
	for idx, member := range members {
		pos[member.ID] = idx
	}
	remaining := map[string]int{}
	dependents := map[string][]string{}
	for _, member := range members {
		for _, w := range waits[member.ID] {
			if _, ok := pos[w]; ok {
				remaining[member.ID]++
				dependents[w] = append(dependents[w], member.ID)
			}
		}
	}
	ready := map[string]bool{}
	for _, member := range members {
		if remaining[member.ID] == 0 {
			ready[member.ID] = true
		}
	}
	done := make(chan int)
	running, stop := 0, false
	start := func(id string) {
		delete(ready, id)
		idx := pos[id]
		started[idx] = true
		running++
		go func(idx int) {
			results[idx] = runWorkspaceMember(ctx, ws, members[idx], selectorPlan)
			done <- idx
		}(idx)
	}
	for {
		for !stop && running < jobs && len(ready) > 0 {
			start(readyOrder(ready)[0])
		}
		if running == 0 && !stop {
			blocked := map[string]int{}
			for idx, member := range members {
				if !started[idx] {
					blocked[member.ID] = remaining[member.ID]
				}
			}
			if len(blocked) > 0 {
				start(breakCycle(blocked))
			}
		}
		if running == 0 {
			break
		}
		idx := <-done
		running--
		if results[idx].Err != nil && ctx.Inv.FailFast {
			stop = true
		}
		if journal != nil {
			if results[idx].Err == nil && !results[idx].Skipped {
				if err := journal.record(members[idx].ID); err != nil {
					ctx.Renderer.Error(Diagnostic{Code: "journal_write_failed", Member: members[idx].ID, Message: err.Error()})
					stop = true
				}
			}
			if !stop && journal.stopRequested() {
				stop, stopped = true, true
				ctx.Renderer.Event(Event{Type: "warning", Message: fmt.Sprintf("stop requested: waiting for %d running member(s), starting no more", running)})
			}
		}
		for _, dep := range dependents[members[idx].ID] {
			remaining[dep]--
			if remaining[dep] == 0 && !started[pos[dep]] {
				ready[dep] = true
			}
		}
	}
	for idx := range members {
		if !started[idx] {
			results[idx] = memberResult{Member: members[idx], Skipped: true}
		}
	}
	return results, stopped
}

func runWorkspaceMember(ctx *Context, ws WorkspaceConfig, member WorkspaceMember, selectorPlan workspaceSelectorPlan) memberResult {
	if reason := selectorPlan.Skip[member.ID]; reason != "" {
		ctx.Renderer.Event(Event{Type: "member_skipped", Member: member.ID, Message: reason})
		return memberResult{Member: member, Skipped: true}
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
	if err != nil {
		ctx.Renderer.Error(Diagnostic{Code: "member_failed", Member: member.ID, Message: err.Error()})
	}
	return memberResult{Member: member, Err: err}
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
