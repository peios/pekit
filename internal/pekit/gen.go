package pekit

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gen and verify are the source-generating half of pekit. A [gen.NAME] target
// runs `command` to write generated source back into the committed tree (its
// product is in-tree, not an artifact), and an optional `verify_command` is the
// drift gate: `pekit verify` runs it on demand, and build/test/package/publish
// run the scoped ones as a pre-flight so you never consume a stale tree.

// genScratchDir is the pekit-managed scratch $PEKIT_OUT for a gen/verify run.
// It lives on the out_dir volume but outside the artifact namespace (build/,
// test/, install/, …), under a dotted path, so package artifact collection and
// --no-build stage reuse never mistake it for a build stage. It is cleaned on
// success and retained on failure for post-mortem.
func genScratchDir(source SourceState, name string) string {
	base := source.WorkBase
	if base == "" {
		base = source.OutBase
	}
	return filepath.Join(base, ".scratch", "gen", name)
}

// runGen executes the `gen` command: run each selected gen target's `command`
// to (re)generate its committed source in place.
func runGen(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, selectors []string, member string) error {
	targets, err := selectGenTargets(recipe, selectors, ctx.Inv.All)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := runGenPhase(ctx, recipe, workspace, source, t, t.Command, t.Dependencies, CommandGen, "gen", member); err != nil {
			return wrapDiag("gen_failed", "gen:"+t.Name, err)
		}
	}
	return nil
}

// runVerify executes the `verify` command: run each selected gen target's
// verify_command (check only, never writes the tree). Failures are collected so
// every out-of-date target is reported in one pass.
func runVerify(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, selectors []string, member string) error {
	targets, err := selectGenTargets(recipe, selectors, ctx.Inv.All)
	if err != nil {
		return err
	}
	explicit := len(selectors) > 0
	var failures []error
	ran := false
	for _, t := range targets {
		if t.VerifyCommand.Empty() {
			if explicit {
				return diag("no_verify_command", "gen target %q has no verify_command", t.Name)
			}
			continue
		}
		ran = true
		if err := runVerifyTarget(ctx, recipe, workspace, source, t, member); err != nil {
			failures = append(failures, wrapDiag("verify_failed", "verify:"+t.Name, err))
		}
	}
	if !ran && !explicit {
		return diag("no_verify_command", "no gen target defines a verify_command")
	}
	return combineFailures(failures)
}

// runVerifyTarget runs one gen target's verify_command, substituting
// verify_dependencies for the dependency payload when present.
func runVerifyTarget(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, t TargetConfig, member string) error {
	deps := t.Dependencies
	if t.VerifyDependencies != nil {
		deps = t.VerifyDependencies
	}
	return runGenPhase(ctx, recipe, workspace, source, t, t.VerifyCommand, deps, CommandVerify, "verify", member)
}

// runGenPhase runs one gen/verify command in the gen target's own environment,
// with a freshly-created scratch $PEKIT_OUT and cwd at the recipe root (gen
// operates on the committed tree, not a resolved source clone).
func runGenPhase(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, source SourceState, t TargetConfig, command ShellCommand, deps map[string]map[string]string, kind Command, phase, member string) error {
	if command.Empty() {
		return diag("empty_command", "gen target %q has no %s command", t.Name, phase)
	}
	scratch := genScratchDir(source, t.Name)
	label := phase + ":" + t.Name
	if ctx.Inv.DryRun {
		ctx.Renderer.Event(Event{Type: phase + "_plan", Member: member, Target: t.Name, Path: scratch, Message: "would run " + label})
		return nil
	}
	if err := os.RemoveAll(scratch); err != nil {
		return wrapDiag("clean_scratch", scratch, err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return wrapDiag("mkdir", scratch, err)
	}
	// Derive an env target: the verify phase reports as PEKIT_COMMAND=verify and
	// carries its own dependency set (verify_dependencies when set), keeping its
	// dependency payload file distinct from the gen phase's.
	envTarget := t
	envTarget.Kind = kind
	envTarget.Dependencies = deps
	env, err := BuildCommandEnv(ctx, recipe, workspace, source, Version{}, envTarget, scratch)
	if err != nil {
		return err
	}
	start := time.Now()
	ctx.Renderer.Event(Event{Type: phase + "_start", Member: member, Target: t.Name, Message: "running " + label})
	if err := executeCommand(ctx, command, env, recipe.Root, member, "", label); err != nil {
		// Retain the scratch dir for post-mortem when a phase fails.
		return err
	}
	ctx.Renderer.Event(Event{Type: phase + "_success", Member: member, Target: t.Name, DurationMS: time.Since(start).Milliseconds(), Message: label + " succeeded"})
	_ = os.RemoveAll(scratch)
	return nil
}

func selectGenTargets(recipe RecipeConfig, selectors []string, all bool) ([]TargetConfig, error) {
	targets := recipe.Targets[CommandGen]
	if len(targets) == 0 {
		return nil, diag("missing_target", "recipe has no gen targets")
	}
	if all {
		out := make([]TargetConfig, 0, len(targets))
		for _, name := range sortedKeys(targets) {
			out = append(out, targets[name])
		}
		return out, nil
	}
	if len(selectors) == 0 {
		if t, ok := targets["main"]; ok {
			return []TargetConfig{t}, nil
		}
		return nil, diag("ambiguous_target", "no gen.main target; name a gen target or pass --all; available: %s", strings.Join(sortedKeys(targets), ", "))
	}
	out := make([]TargetConfig, 0, len(selectors))
	for _, name := range selectors {
		if err := validateSelector("target", name); err != nil {
			return nil, err
		}
		t, ok := targets[name]
		if !ok {
			return nil, diag("missing_target", "unknown gen target %q; available: %s", name, strings.Join(sortedKeys(targets), ", "))
		}
		out = append(out, t)
	}
	return out, nil
}

// preflightVerify runs the drift gates that guard a consuming command
// (build/test/install/package/publish). A gen target's verify_command fires
// when its verify_on_build/verify_on_test scope gates a target this invocation
// will run; failures are collected so every stale generator is reported at
// once. --no-verify (bare) skips the pre-flight entirely; --no-verify=a,b skips
// the named gens.
func preflightVerify(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, member string) error {
	gens := recipe.Targets[CommandGen]
	skipAll, skip, err := parseNoVerify(ctx.Inv, gens)
	if err != nil {
		return err
	}
	if skipAll || len(gens) == 0 {
		return nil
	}
	cmd := ctx.Inv.EffectiveCommand()
	buildNames, testNames, err := plannedGating(recipe, cmd, ctx.Inv.selectors())
	if err != nil {
		return err
	}
	source := cleanSourceState(recipe)
	var failures []error
	for _, name := range sortedKeys(gens) {
		g := gens[name]
		if g.VerifyCommand.Empty() || !genFires(g, buildNames, testNames) {
			continue
		}
		if skip[name] {
			ctx.Renderer.Event(Event{Type: "verify_skipped", Member: member, Target: name, Message: "pre-flight verify skipped (--no-verify)"})
			continue
		}
		if err := runVerifyTarget(ctx, recipe, workspace, source, g, member); err != nil {
			failures = append(failures, preflightError(name, cmd, err))
		}
	}
	return combineFailures(failures)
}

// genFires reports whether gen target g's verify_command should run given the
// build and test target names this invocation will execute.
func genFires(g TargetConfig, buildNames, testNames []string) bool {
	if len(buildNames) > 0 && gates(g.VerifyOnBuild, buildNames) {
		return true
	}
	if len(testNames) > 0 && gates(g.VerifyOnTest, testNames) {
		return true
	}
	return false
}

// gates applies the scope semantics for one namespace: a nil scope (absent)
// gates every target in that namespace; a non-nil scope gates only the listed
// names (so an empty scope gates nothing).
func gates(scope *[]string, running []string) bool {
	if scope == nil {
		return true
	}
	set := stringSet(*scope)
	for _, n := range running {
		if set[n] {
			return true
		}
	}
	return false
}

// plannedGating computes the build and test target names a consuming command
// will run, used to scope the pre-flight. build/test/install resolve the real
// build closure; package/publish gate conservatively against every build
// target (the exact set depends on package resolution, which needs a resolved
// source — over-verifying the ship path is the safe bias).
func plannedGating(recipe RecipeConfig, cmd Command, selectors []string) (buildNames, testNames []string, err error) {
	builds := recipe.Targets[CommandBuild]
	switch cmd {
	case CommandBuild:
		roots, err := selectTargets(CommandBuild, builds, selectors)
		if err != nil {
			return nil, nil, err
		}
		order, err := topoBuilds(builds, roots)
		if err != nil {
			return nil, nil, err
		}
		return targetNames(order), nil, nil
	case CommandTest, CommandInstall:
		selected, err := selectTargets(cmd, recipe.Targets[cmd], selectors)
		if err != nil {
			return nil, nil, err
		}
		var roots []TargetConfig
		for _, t := range selected {
			for _, dep := range t.Needs {
				if bt, ok := builds[dep]; ok {
					roots = append(roots, bt)
				}
			}
		}
		order, err := topoBuilds(builds, roots)
		if err != nil {
			return nil, nil, err
		}
		if cmd == CommandTest {
			return targetNames(order), targetNames(selected), nil
		}
		return targetNames(order), nil, nil
	case CommandPackage, CommandPublish:
		out := make([]TargetConfig, 0, len(builds))
		for _, name := range sortedKeys(builds) {
			out = append(out, builds[name])
		}
		return targetNames(out), nil, nil
	default:
		return nil, nil, nil
	}
}

func targetNames(targets []TargetConfig) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Name)
	}
	return out
}

// parseNoVerify interprets --no-verify. A nil flag means verify normally; a
// bare flag (empty value) skips all pre-flight verifies; a value names the gen
// targets to skip. Named targets must exist.
func parseNoVerify(inv Invocation, gens map[string]TargetConfig) (skipAll bool, skip map[string]bool, err error) {
	skip = map[string]bool{}
	if inv.NoVerify == nil {
		return false, skip, nil
	}
	if strings.TrimSpace(*inv.NoVerify) == "" {
		return true, skip, nil
	}
	for _, item := range strings.Split(*inv.NoVerify, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		if _, ok := gens[name]; !ok {
			return false, nil, diag("missing_target", "--no-verify names unknown gen target %q", name)
		}
		skip[name] = true
	}
	return false, skip, nil
}

// preflightError wraps a failed pre-flight verify with the remediation the user
// wants: regenerate with `pekit gen`, or bypass with `--no-verify`.
func preflightError(name string, cmd Command, err error) error {
	return diag("gen_out_of_date",
		"gen target %q is out of date: %s\n  fix:  pekit gen %s\n  skip: pekit %s --no-verify=%s",
		name, err.Error(), name, cmd, name)
}

// combineFailures collapses collected verify failures into one error, listing
// every failure so a single run surfaces all stale targets.
func combineFailures(failures []error) error {
	switch len(failures) {
	case 0:
		return nil
	case 1:
		return failures[0]
	default:
		msgs := make([]string, 0, len(failures))
		for _, f := range failures {
			msgs = append(msgs, f.Error())
		}
		return diag("verify_failed", "%d verify checks failed:\n%s", len(failures), strings.Join(msgs, "\n"))
	}
}
