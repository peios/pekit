package pekit

import (
	"fmt"
	"sort"
	"strings"
)

// lint is pekit's rule checker. `pekit lint` reads the recipe, package files,
// lock and patches against the rules the recipe's lint.pekit.toml chain
// enables, and reports every finding in one pass. A delegated recipe also
// materialises one source snapshot and statically checks the effective merged
// recipe. With an explicit version/source selector lint additionally checks
// the payload each package would pack from an existing build stage. It never
// runs a build.
//
// Findings are errors: a rule is either on, or exempted in [allow] with a
// reason. There is no warning level — a warning is a finding nobody has to
// justify.

// lintFinding is one rule violation, attributed to a package and a path
// where the rule has one.
type lintFinding struct {
	Rule    string
	Package string
	Path    string
	Message string
	Reason  string // set when [allow] exempted it
}

// linter collects findings for one recipe run.
type linter struct {
	ctx       *Context
	cfg       LintConfig
	member    string
	findings  []lintFinding
	allowed   []lintFinding
	usedAllow map[string]bool
}

func newLinter(ctx *Context, cfg LintConfig, member string) *linter {
	return &linter{ctx: ctx, cfg: cfg, member: member, usedAllow: map[string]bool{}}
}

// report records a finding for rule, or an exemption when [allow] names
// the rule, and renders it as it happens.
func (l *linter) report(rule, pkg, path, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	f := lintFinding{Rule: rule, Package: pkg, Path: path, Message: msg}
	if allow, ok := l.cfg.Allow[rule]; ok {
		f.Reason = allow.Reason
		l.allowed = append(l.allowed, f)
		l.usedAllow[rule] = true
		l.ctx.Renderer.Event(Event{Type: "lint_allowed", Member: l.member, Package: pkg, Path: path, Rule: rule,
			Message: fmt.Sprintf("%s: %s (allowed: %s)", rule, msg, allow.Reason)})
		return
	}
	l.findings = append(l.findings, f)
	l.ctx.Renderer.Event(Event{Type: "lint", Member: l.member, Package: pkg, Path: path, Rule: rule,
		Message: rule + ": " + msg})
}

// on reports whether a rule is enabled; rules call it before doing any work.
func (l *linter) on(rule string) bool { return l.cfg.Enabled(rule) }

// finish emits the summary and turns findings into the command's error.
func (l *linter) finish() error {
	var unused []string
	for id, allow := range l.cfg.Allow {
		if !l.usedAllow[id] {
			unused = append(unused, id+" ("+allow.Path+")")
		}
	}
	sort.Strings(unused)
	for _, u := range unused {
		l.ctx.Renderer.Event(Event{Type: "lint_unused_allow", Member: l.member, Message: "allow entry never applied: " + u})
	}
	summary := fmt.Sprintf("%d finding(s), %d allowed", len(l.findings), len(l.allowed))
	if len(unused) > 0 {
		summary += fmt.Sprintf(", %d unused allow entr%s", len(unused), plural(len(unused), "y", "ies"))
	}
	l.ctx.Renderer.Event(Event{Type: "lint_summary", Member: l.member, Message: summary})
	if len(l.findings) == 0 {
		return nil
	}
	rules := map[string]int{}
	for _, f := range l.findings {
		rules[f.Rule]++
	}
	parts := make([]string, 0, len(rules))
	for _, id := range sortedKeys(rules) {
		parts = append(parts, fmt.Sprintf("%s (%d)", id, rules[id]))
	}
	return diag("lint_failed", "%d lint finding(s): %s", len(l.findings), strings.Join(parts, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// runLint is the `lint` command for one recipe.
func runLint(ctx *Context, recipe RecipeConfig, workspace *WorkspaceConfig, member string) error {
	cfg, err := loadLintConfig(recipe.Root, workspace, "")
	if err != nil {
		return err
	}
	l := newLinter(ctx, cfg, member)
	explicitSource := lintHasExplicitSourceSelection(ctx.Inv)
	resolveDelegate := recipe.Delegate.Any() && recipe.Source.HasExternal()
	if !explicitSource && !resolveDelegate {
		if len(cfg.Files) == 0 {
			return missingLintConfig(recipe.Root)
		}
		emitLintConfig(ctx, member, cfg)
		if err := lintStatic(l, recipe, workspace, cleanSourceState(recipe)); err != nil {
			return err
		}
		if payload := cfg.enabledPayloadRules(); len(payload) > 0 {
			ctx.Renderer.Event(Event{Type: "lint_skipped", Member: member,
				Message: fmt.Sprintf("%d payload rule(s) need a staged build: pass --version (or --latest/--local) to check an existing stage", len(payload))})
		}
		return l.finish()
	}

	resolveCtx := ctx
	if resolveDelegate && !explicitSource {
		resolveCtx = delegatedLintContext(ctx, recipe.Source)
	}
	versions, err := resolveRecipeVersions(resolveCtx, recipe)
	if err != nil {
		return err
	}
	for _, version := range versions {
		source, err := ResolveSource(resolveCtx, recipe, version)
		if err != nil {
			return err
		}
		effective, err := mergeDelegatedRecipe(recipe, source)
		if err != nil {
			return err
		}
		// A delegated source may carry its own lint file; reload with it in
		// the chain so the project's own bar applies to its payload.
		if source.SourceRoot != "" && source.SourceRoot != recipe.Root {
			reloaded, err := loadLintConfig(recipe.Root, workspace, source.SourceRoot)
			if err != nil {
				return err
			}
			l.cfg = reloaded
		}
		if len(l.cfg.Files) == 0 {
			return missingLintConfig(recipe.Root)
		}
		emitLintConfig(ctx, member, l.cfg)
		if err := lintStatic(l, effective, workspace, source); err != nil {
			return err
		}
		if explicitSource {
			if err := lintPayload(l, effective, workspace, source, version); err != nil {
				return err
			}
		}
	}
	if !explicitSource {
		if payload := l.cfg.enabledPayloadRules(); len(payload) > 0 {
			ctx.Renderer.Event(Event{Type: "lint_skipped", Member: member,
				Message: fmt.Sprintf("%d payload rule(s) need a staged build: pass --version (or --latest/--local) to check an existing stage", len(payload))})
		}
	}
	return l.finish()
}

func missingLintConfig(recipeRoot string) error {
	return diag("missing_lint_config", "no %s found for %s: pekit enables no rules by default, so there is nothing to lint", lintFileName, recipeRoot)
}

func emitLintConfig(ctx *Context, member string, cfg LintConfig) {
	if ctx.Inv.Verbose {
		ctx.Renderer.Event(Event{Type: "lint_config", Member: member, Message: "lint files: " + strings.Join(cfg.Files, ", ") + "; rules: " + strings.Join(cfg.EnabledRules(), ", ")})
	}
}

func lintHasExplicitSourceSelection(inv Invocation) bool {
	return inv.Version != "" || inv.Latest || inv.AllVersions || inv.Local != nil || inv.PreferLocal != nil
}

// delegatedLintContext chooses one source snapshot for a plain lint. A local-
// only delegate uses its configured local tree. Reproducible sources whose
// materialisation needs a version select their newest available version;
// fixed refs/URLs can be acquired without inventing a version selector.
func delegatedLintContext(ctx *Context, source SourceConfig) *Context {
	localCtx := *ctx
	localCtx.Inv = ctx.Inv
	if !source.HasReproducible() {
		local := ""
		localCtx.Inv.Local = &local
		return &localCtx
	}
	needsVersion := source.PyPI.Project != "" || source.Git.TrackedPath != "" ||
		hasVersionTemplate(
			source.Git.Ref,
			source.URL.URL,
			source.URL.Root,
			source.URL.Signature.URL,
			source.URL.PatchSeries.URL,
			source.URL.PatchSeries.Signature.URL,
		)
	if needsVersion {
		localCtx.Inv.Latest = true
	}
	return &localCtx
}

func hasVersionTemplate(values ...string) bool {
	for _, value := range values {
		for _, name := range []string{
			"version", "major", "minor", "patch", "revision",
			"revision_suffix", "suffix", "prerelease", "buildmeta",
		} {
			if strings.Contains(value, "{{"+name+"}}") {
				return true
			}
		}
	}
	return false
}

// enabledPayloadRules lists the enabled rules that need a staged payload.
func (c LintConfig) enabledPayloadRules() []string {
	var out []string
	for _, k := range lintKeys {
		if k.Payload && !k.Param && c.Enabled(k.ID) {
			out = append(out, k.ID)
		}
	}
	return out
}
