package pekit

import (
	"sort"
	"strings"
)

// Workspace member selection by tag, and dependency ordering.
//
// Ordering gives a batch its best chance of succeeding from an empty
// repository: a member waits for the members that produce what its gate
// targets install. It is a schedule, not a correctness proof. Anything a
// member needs that no selected member produces resolves as before, from
// whatever the dependency provider already has, and members in a dependency
// cycle are started as late as possible: only when nothing else can run does
// the scheduler start the blocked member with the fewest unfinished
// dependencies, so a cycle is broken one member at a time. Breaking a real
// cycle properly is the job of a separately built seed (see --tag).

// filterMembersByTag keeps the members that carry any of inv.Tags (every
// member when none are given) and none of inv.ExcludeTags. Tags come only
// from the member recipe itself.
func filterMembersByTag(inv Invocation, members []WorkspaceMember) ([]WorkspaceMember, error) {
	if len(inv.Tags) == 0 && len(inv.ExcludeTags) == 0 {
		return members, nil
	}
	want := stringSet(inv.Tags)
	reject := stringSet(inv.ExcludeTags)
	var out []WorkspaceMember
	for _, member := range members {
		recipe, err := LoadRecipe(member.RecipePath)
		if err != nil {
			return nil, err
		}
		keep := len(want) == 0
		excluded := false
		for _, tag := range recipe.Tags {
			if want[tag] {
				keep = true
			}
			if reject[tag] {
				excluded = true
			}
		}
		if keep && !excluded {
			out = append(out, member)
		}
	}
	if len(out) == 0 {
		return nil, diag("empty_workspace", "no workspace member matches the requested tags")
	}
	return out, nil
}

// memberDependencyFacts is what ordering needs from one member: the names
// its gate targets install, and the packages it defines with their
// provider-scoped runtime dependencies.
type memberDependencyFacts struct {
	needs    map[string]bool
	produces map[string]bool
	runtime  map[string]map[string]bool
}

// gateTargetKinds are the target kinds whose dependencies a delegated
// command installs before it can succeed.
func gateTargetKinds(inv Invocation) (build, tests bool, gatedOnly bool, ok bool) {
	switch inv.DelegateCommand {
	case CommandBuild, CommandInstall:
		return true, false, false, true
	case CommandTest:
		return true, true, false, true
	case CommandPackage, CommandPublish:
		return true, !inv.NoGates, true, true
	}
	return false, false, false, false
}

// planWorkspaceOrder returns, for each member id, the ids it must wait for.
// A nil map means members are independent (no dependency provider, a
// command without dependencies, or a single member).
func planWorkspaceOrder(ctx *Context, ws WorkspaceConfig, members []WorkspaceMember, selectorPlan workspaceSelectorPlan) (map[string][]string, error) {
	build, tests, gatedOnly, ok := gateTargetKinds(ctx.Inv)
	if !ok || len(members) < 2 {
		return nil, nil
	}
	profile, err := selectedEnvFile(ctx.Inv, ws.Root, true)
	if err != nil {
		return nil, err
	}
	provider := profile.DependencyProvider
	if provider == "" {
		return nil, nil
	}
	facts := map[string]memberDependencyFacts{}
	for _, member := range members {
		if selectorPlan.Skip[member.ID] != "" {
			continue
		}
		facts[member.ID] = memberFacts(ctx, ws, member, provider, build, tests, gatedOnly)
	}
	producer := map[string][]string{}
	runtime := map[string]map[string]bool{}
	for id, f := range facts {
		for name := range f.produces {
			producer[name] = append(producer[name], id)
		}
		for name, deps := range f.runtime {
			if runtime[name] == nil {
				runtime[name] = map[string]bool{}
			}
			for dep := range deps {
				runtime[name][dep] = true
			}
		}
	}
	edges := map[string]map[string]bool{}
	for id, f := range facts {
		edges[id] = map[string]bool{}
		seen := map[string]bool{}
		queue := sortedKeys(f.needs)
		for len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			if seen[name] {
				continue
			}
			seen[name] = true
			for _, m := range producer[name] {
				if m != id {
					edges[id][m] = true
				}
			}
			for dep := range runtime[name] {
				queue = append(queue, dep)
			}
		}
	}
	waits := map[string][]string{}
	for id, deps := range edges {
		waits[id] = sortedKeys(deps)
	}
	return waits, nil
}

// memberFacts reads one member's effective recipe for every selected
// version. It resolves sources exactly as the member's own run will (and
// shares its cache), so a delegated recipe's targets and packages are known;
// under --dry-run only already-cached sources are visible. It is best
// effort: a member whose recipe or source cannot be resolved contributes no
// edges and reports the failure when it runs.
func memberFacts(ctx *Context, ws WorkspaceConfig, member WorkspaceMember, provider string, build, tests, gatedOnly bool) memberDependencyFacts {
	f := memberDependencyFacts{needs: map[string]bool{}, produces: map[string]bool{}, runtime: map[string]map[string]bool{}}
	memberInv := ctx.Inv
	memberInv.Command = memberInv.DelegateCommand
	memberInv.WorkspaceMode = false
	memberInv.WorkspaceFlag = ws.Path
	memberInv.SuppressUnsupportedVersion = memberInv.AllowUnused
	// The member's own run refreshes a source; resolving it twice would
	// fetch twice.
	memberInv.RefreshSource = false
	memberCtx := *ctx
	memberCtx.Inv = memberInv
	recipe, err := LoadRecipe(member.RecipePath)
	if err != nil {
		return f
	}
	versions, err := resolveRecipeVersions(&memberCtx, recipe)
	if err != nil {
		return f
	}
	for _, version := range versions {
		source, err := ResolveSource(&memberCtx, recipe, version)
		if err != nil {
			continue
		}
		effective, err := mergeDelegatedRecipe(recipe, source)
		if err != nil {
			continue
		}
		var kinds []Command
		if build {
			kinds = append(kinds, CommandBuild)
		}
		if tests {
			kinds = append(kinds, CommandTest)
		}
		for _, kind := range kinds {
			for _, target := range effective.Targets[kind] {
				if kind == CommandTest && gatedOnly && !target.Gate {
					continue
				}
				for name := range target.Dependencies[provider] {
					f.needs[name] = true
				}
			}
		}
		packages, err := loadEffectivePackages(effective, &ws, source)
		if err != nil {
			continue
		}
		for _, pkg := range packages {
			instances, err := expandPackageInstances(pkg, source, effective, &ws, version)
			if err != nil {
				continue
			}
			for _, inst := range instances {
				names := append([]string{inst.Name}, sortedKeys(inst.Config.Package.Provides)...)
				for _, name := range names {
					f.produces[name] = true
				}
				if inst.Config.DependencyProvider != provider {
					continue
				}
				for _, name := range names {
					if f.runtime[name] == nil {
						f.runtime[name] = map[string]bool{}
					}
					for dep := range inst.Config.Package.Dependencies {
						f.runtime[name][dep] = true
					}
				}
			}
		}
	}
	return f
}

// breakCycle picks the member to start when nothing is runnable but members
// remain, which only a dependency cycle can cause: the blocked member with
// the fewest unfinished dependencies, lowest id first.
func breakCycle(blocked map[string]int) string {
	best := ""
	for _, id := range readyOrder(stringSetKeys(blocked)) {
		if best == "" || blocked[id] < blocked[best] {
			best = id
		}
	}
	return best
}

func stringSetKeys(m map[string]int) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// describeWaits renders one member's waits for dry-run output.
func describeWaits(waits []string) string {
	if len(waits) == 0 {
		return "no workspace dependencies"
	}
	return "after " + strings.Join(waits, ", ")
}

// readyOrder is the dispatch order among currently runnable members: ids in
// lexicographic order, so a schedule is deterministic.
func readyOrder(ready map[string]bool) []string {
	out := make([]string, 0, len(ready))
	for id := range ready {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
