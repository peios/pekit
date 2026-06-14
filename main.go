package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const usage = "usage: pekit <build|test|install|clean> [target] | pekit <package|publish> [name] [--local] [--no-build] | pekit workspace <package|publish> <--all|--latest|--local>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pekit: %v\n", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	// `workspace` is a fan-out prefix with its own flags (--all/--latest),
	// not the per-recipe verb flags, so it is handled before extractFlags.
	if len(args) > 0 && args[0] == "workspace" {
		return cmdWorkspace(args[1:])
	}

	args, f, err := extractFlags(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New(usage)
	}

	// --no-build trusts the already-staged build tree, so it only means
	// anything to the verbs that stage-and-pack.
	if f.noBuild && args[0] != "package" && args[0] != "publish" {
		return fmt.Errorf("--no-build only applies to package and publish")
	}

	// Local mode: PREFER the [source] working copy, fall back to remote.
	// Source selection (--local) and version numbering (--version) are
	// independent — --version only restamps and never drags a local build
	// back to git. The local version defaults to a sentinel that sorts
	// below every release; the ledger is off (dev builds aren't recorded).
	// If the working copy isn't present, rather than fail we build from git
	// (the named --version, else the latest tag) with a warning — only a
	// recipe with no git fallback errors.
	if f.local {
		if f.remember {
			return fmt.Errorf("--remember-built does not apply to --local (dev builds aren't recorded)")
		}
		src, serr := loadRecipeSource()
		if serr != nil {
			return serr
		}
		if src == nil {
			return fmt.Errorf("--local needs a [source]")
		}
		exactVersion := func() (*Version, error) {
			if !f.hasVersion {
				return nil, nil
			}
			v, perr := parseVersion(f.version)
			if perr != nil {
				return nil, fmt.Errorf("with --local, --version must be an exact version: %w", perr)
			}
			return v, nil
		}

		if localUsable(src) {
			ver, verr := exactVersion()
			if verr != nil {
				return verr
			}
			if ver == nil {
				ver = localVersion()
			}
			return dispatch(args, ver, true, f.noBuild)
		}

		// Working copy not present — fall back to remote instead of failing.
		if src.Git == "" {
			return fmt.Errorf("--local: %s and no git source to fall back to", localMissReason(src))
		}
		ver, verr := exactVersion()
		if verr != nil {
			return verr
		}
		if ver == nil {
			v, lerr := latestVersion()
			if lerr != nil {
				return fmt.Errorf("--local fallback to remote: %w", lerr)
			}
			ver = v
		}
		fmt.Fprintf(os.Stderr, "pekit: warning: --local: %s; building %s from remote\n", localMissReason(src), ver.Full)
		return dispatch(args, ver, false, f.noBuild)
	}

	// pekit.built is consulted for the work-doing verbs (build/package/
	// publish); test/clean ignore it.
	ledgerActive := args[0] == "build" || args[0] == "package" || args[0] == "publish"
	if f.remember && !ledgerActive {
		return fmt.Errorf("--remember-built only applies to build, package and publish")
	}
	if f.remember && !f.hasVersion {
		return fmt.Errorf("--remember-built needs --version (nothing to record otherwise)")
	}

	vers, err := resolveVersions(f.version, f.hasVersion)
	if err != nil {
		return err
	}

	// The ledger is authoritative just by existing: any version it lists
	// is skipped (--bust overrides). An absent file is an empty ledger,
	// so this is a no-op until something has been recorded. Recording is
	// the separate, opt-in act (--remember-built).
	var ledger *builtLedger
	if ledgerActive {
		if ledger, err = loadLedger("pekit.built"); err != nil {
			return err
		}
	}
	skip := func(v *Version) bool {
		return ledger != nil && v != nil && !f.bust && ledger.has(v.Full)
	}

	// Plain single version (or none) that won't be recorded: dispatch
	// directly so a child's exit code propagates unchanged.
	if len(vers) == 1 && !f.remember {
		if skip(vers[0]) {
			fmt.Fprintf(os.Stderr, "pekit: %s already built, skipping (--bust to rebuild)\n", vers[0].Full)
			return nil
		}
		return dispatch(args, vers[0], false, f.noBuild)
	}

	if len(vers) > 1 {
		labels := make([]string, len(vers))
		for i, v := range vers {
			labels[i] = v.Full
		}
		fmt.Fprintf(os.Stderr, "pekit: %s → %d versions: %s\n", f.version, len(vers), strings.Join(labels, ", "))
	}

	var failed []string
	built, skipped := 0, 0
	for _, ver := range vers {
		if skip(ver) {
			fmt.Fprintf(os.Stderr, "pekit: %s already built, skipping (--bust to rebuild)\n", ver.Full)
			skipped++
			continue
		}
		if err := dispatch(args, ver, false, f.noBuild); err != nil {
			failed = append(failed, ver.Full)
			fmt.Fprintf(os.Stderr, "pekit: %s failed: %v\n", ver.Full, err)
			continue
		}
		if f.remember {
			if err := ledger.add(ver.Full); err != nil {
				return fmt.Errorf("recording %s in pekit.built: %w", ver.Full, err)
			}
		}
		built++
	}
	if f.remember || skipped > 0 {
		fmt.Fprintf(os.Stderr, "pekit: %d built, %d skipped, %d failed\n", built, skipped, len(failed))
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d versions failed: %s", len(failed), len(vers), strings.Join(failed, ", "))
	}
	return nil
}

// cmdWorkspace fans a verb out over the members of a workspace. Version
// selection is workspace-level (--all = every tracked version per member,
// --latest = the newest per member) rather than the per-recipe --version,
// since each member has its own version space. Each member is run via the
// normal per-recipe path (in its own directory, with its own pekit.built),
// so this is pure orchestration over the existing verbs.
func cmdWorkspace(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pekit workspace <package|publish> <--all|--latest|--local> [--remember-built] [--bust]")
	}
	verb := args[0]
	if verb != "package" && verb != "publish" {
		return fmt.Errorf("pekit workspace supports package and publish (got %q)", verb)
	}

	var all, latest, local, remember, bust bool
	for _, a := range args[1:] {
		switch a {
		case "--all":
			all = true
		case "--latest":
			latest = true
		case "--local":
			local = true
		case "--remember-built":
			remember = true
		case "--bust":
			bust = true
		default:
			return fmt.Errorf("pekit workspace: unknown flag %q", a)
		}
	}
	modes := 0
	for _, on := range []bool{all, latest, local} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		return fmt.Errorf("pekit workspace %s needs exactly one of --all, --latest, or --local", verb)
	}
	if local && remember {
		return fmt.Errorf("--remember-built does not apply to --local (dev builds aren't recorded)")
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, found, err := findWorkspaceRoot(wd)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no workspace.pekit.toml at or above %s", wd)
	}
	ws, err := LoadWorkspace(filepath.Join(root, "workspace.pekit.toml"))
	if err != nil {
		return err
	}
	members, err := workspaceMembers(root, ws)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		fmt.Fprintf(os.Stderr, "pekit: workspace %s: no members match %q\n", verb, ws.Include)
		return nil
	}

	names := make([]string, len(members))
	for i, m := range members {
		names[i] = filepath.Base(m)
	}
	mode := "all"
	if latest {
		mode = "latest"
	}
	if local {
		mode = "local"
	}
	fmt.Fprintf(os.Stderr, "pekit: workspace %s (--%s): %d members: %s\n",
		verb, mode, len(members), strings.Join(names, ", "))

	var failed []string
	for _, m := range members {
		name := filepath.Base(m)
		fmt.Fprintf(os.Stderr, "pekit: ── %s ──\n", name)
		err := inDir(m, func() error {
			if local {
				return run([]string{verb, "--local"})
			}
			// Sourceless members (a buildless package like fsbase/live-boot,
			// whose version is pinned in its package file) have no upstream
			// tags to enumerate, so --all/--latest don't apply: build the one
			// fixed version with a bare verb (no --version renders the package
			// file as-is). The ledger flags key on enumerated versions and
			// likewise don't apply.
			src, serr := loadRecipeSource()
			if serr != nil {
				return serr
			}
			if src == nil {
				if remember {
					fmt.Fprintf(os.Stderr, "pekit: %s: no [source]; building its fixed version (--remember-built does not apply)\n", name)
				}
				return run([]string{verb})
			}
			sel := "*"
			if latest {
				v, lerr := latestVersion()
				if lerr != nil {
					return lerr
				}
				sel = v.Full
			}
			inner := []string{verb, "--version", sel}
			if remember {
				inner = append(inner, "--remember-built")
			}
			if bust {
				inner = append(inner, "--bust")
			}
			return run(inner)
		})
		if err != nil {
			failed = append(failed, name)
			fmt.Fprintf(os.Stderr, "pekit: %s failed: %v\n", name, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d members failed: %s", len(failed), len(members), strings.Join(failed, ", "))
	}
	return nil
}

// inDir runs fn with the process working directory changed to dir, then
// restores it. pekit's per-recipe path is all cwd-relative, so this is how
// the workspace fan-out runs each member through the normal verbs.
func inDir(dir string, fn func() error) error {
	prev, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer os.Chdir(prev)
	return fn()
}

func dispatch(args []string, ver *Version, local, noBuild bool) error {
	switch args[0] {
	case "build", "test", "install":
		return cmdVerb(args[0], args[1:], ver, local)
	case "package":
		return cmdPackage(args[1:], ver, local, noBuild)
	case "publish":
		return cmdPublish(args[1:], ver, local, noBuild)
	case "clean":
		return cmdClean(args[1:], ver)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

// flags are the global options parsed before the verb.
type flags struct {
	version    string // --version/-V value (may be a comma-list of selectors)
	hasVersion bool
	remember   bool // --remember-built: skip + record against pekit.built
	bust       bool // --bust: rebuild even if the ledger says built
	local      bool // --local: build the [source] localpath working copy
	noBuild    bool // --no-build: reuse already-staged builds; build only what is missing
}

// extractFlags pulls the global flags out of args, returning the rest.
func extractFlags(args []string) ([]string, flags, error) {
	var rest []string
	var f flags
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--version" || a == "-V":
			if i+1 >= len(args) {
				return nil, f, fmt.Errorf("%s requires a value", a)
			}
			f.version, f.hasVersion = args[i+1], true
			i++
		case strings.HasPrefix(a, "--version="):
			f.version, f.hasVersion = strings.TrimPrefix(a, "--version="), true
		case a == "--remember-built":
			f.remember = true
		case a == "--bust":
			f.bust = true
		case a == "--local":
			f.local = true
		case a == "--no-build":
			f.noBuild = true
		default:
			// An unrecognised --flag is a mistake, not a positional: catch it
			// here so it can't be silently swallowed as a target or package
			// name (which produced a baffling "no such package" error).
			if strings.HasPrefix(a, "-") {
				return nil, f, fmt.Errorf("unknown flag %q\n%s", a, usage)
			}
			rest = append(rest, a)
		}
	}
	return rest, f, nil
}

func targetArg(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "main", nil
	case 1:
		return args[0], nil
	default:
		return "", errors.New(usage)
	}
}

func cmdVerb(verb string, args []string, ver *Version, local bool) error {
	name, err := targetArg(args)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return err
	}
	if err := applyLocal(cfg, local); err != nil {
		return err
	}

	targets, ok := cfg.Commands[verb]
	if !ok {
		return fmt.Errorf("pekit.toml has no [%s] section", verb)
	}
	if _, ok := targets[name]; !ok {
		return fmt.Errorf("no %s target %q (available: %s)",
			verb, name, strings.Join(sortedNames(targets), ", "))
	}
	return runTarget(cfg, verb, name, nil, false)
}

// buildOrder returns root and its transitive dependencies in run order
// (dependencies first, each appearing once). The graph is validated acyclic at
// parse time, so this never loops; root must exist in targets.
func buildOrder(targets map[string]Target, root string) []string {
	var order []string
	visited := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if visited[n] {
			return
		}
		visited[n] = true
		for _, dep := range targets[n].Needs {
			visit(dep)
		}
		order = append(order, n)
	}
	visit(root)
	return order
}

// runTarget runs target `name` in section `verb` after its dependencies, in
// dependency order. ran tracks targets already handled this invocation so a
// dependency shared by several targets runs once; pass nil for a one-shot run
// (buildOrder still dedups within the single graph walk).
//
// When skipStaged is set (--no-build), a target whose stage dir already exists
// is reused as-is instead of rebuilt; a target that has never been built is
// still built. So --no-build is purely a "don't rebuild" switch — on a clean
// tree it builds everything, on a warm one it builds only what is missing.
func runTarget(cfg *Config, verb, name string, ran map[string]bool, skipStaged bool) error {
	targets := cfg.Commands[verb]
	for _, t := range buildOrder(targets, name) {
		if ran[t] {
			continue
		}
		if skipStaged {
			if _, err := os.Stat(filepath.Join(outBase(cfg), verb, t)); err == nil {
				fmt.Printf("pekit: %s %s: reusing staged output (--no-build)\n", verb, t)
				if ran != nil {
					ran[t] = true
				}
				continue
			}
		}
		if err := runCommandTarget(cfg, verb, t, targets[t]); err != nil {
			return err
		}
		if ran != nil {
			ran[t] = true
		}
	}
	return nil
}

func runCommandTarget(cfg *Config, verb, name string, target Target) error {
	script := target.Command
	if len(cfg.Env) > 0 {
		script = envPrelude(cfg.Env) + script
		fmt.Printf("pekit: env: %s\n", strings.Join(envNames(cfg.Env), ", "))
	}
	fmt.Printf("pekit: %s %s: %s\n", verb, name, target.Command)
	// -eu so multi-line commands stop at the first failure instead of
	// barrelling on (e.g. staging a stale binary after a failed compile).
	cmd := exec.Command("sh", "-euc", script)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if verb == "build" && cfg.OutDir != "" {
		if cfg.Source != nil {
			srcDir, err := fetchSource(cfg.Source, sourceCheckout(cfg))
			if err != nil {
				return err
			}
			cmd.Dir = srcDir
		}
		stageDir, err := prepareOutDir(outBase(cfg), verb, name, cfg.ClearOut)
		if err != nil {
			return err
		}
		fmt.Printf("pekit: out: %s\n", stageDir)
		cmd.Env = append(os.Environ(), "PEKIT_OUT="+stageDir)
		// Each declared dependency's stage is exported as PEKIT_<NAME>_OUT.
		// runTarget has already built them, so the dirs exist.
		for _, dep := range target.Needs {
			depStage, derr := filepath.Abs(filepath.Join(outBase(cfg), verb, dep))
			if derr != nil {
				return derr
			}
			cmd.Env = append(cmd.Env, "PEKIT_"+envTargetName(dep)+"_OUT="+depStage)
		}
		if len(target.Needs) > 0 {
			fmt.Printf("pekit: needs: %s\n", strings.Join(target.Needs, ", "))
		}
		if err := cmd.Run(); err != nil {
			return err
		}
		if isEmptyDir(stageDir) {
			fmt.Fprintf(os.Stderr, "pekit: warning: build %s left %s empty\n", name, stageDir)
		}
		return nil
	}

	return cmd.Run()
}

// revScope is the filesystem-safe form of a source rev ('/' flattened).
// Local mode has no rev, so its builds share one "localdev" scope.
func revScope(src *Source) string {
	if src.Local {
		return "localdev"
	}
	return strings.ReplaceAll(src.Rev, "/", "_")
}

// localUsable reports whether a source's localpath names an existing
// directory — the condition for --local to build it in place rather than
// fall back to remote.
func localUsable(src *Source) bool {
	if src.LocalPath == "" {
		return false
	}
	abs, err := filepath.Abs(src.LocalPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	return err == nil && info.IsDir()
}

// localMissReason explains why --local could not use a working copy, for
// the fallback warning.
func localMissReason(src *Source) string {
	if src.LocalPath == "" {
		return "recipe has no localpath"
	}
	return fmt.Sprintf("no working copy at %s", src.LocalPath)
}

// applyLocal switches a recipe's source to local mode (build the working
// copy in place). It is the single validation point for --local.
func applyLocal(cfg *Config, local bool) error {
	if !local {
		return nil
	}
	if cfg.Source == nil {
		return fmt.Errorf("--local needs a [source] with a localpath")
	}
	if cfg.Source.LocalPath == "" {
		return fmt.Errorf("--local: [source] has no localpath")
	}
	cfg.Source.Local = true
	return nil
}

// outBase is the staging root. In source mode everything for one rev is
// scoped under OutDir/<rev>/ (source, build, package) so builds of
// different revs coexist; without a source it's just OutDir.
func outBase(cfg *Config) string {
	if cfg.Source != nil {
		return filepath.Join(cfg.OutDir, revScope(cfg.Source))
	}
	return cfg.OutDir
}

// sourceCheckout is where [source] is checked out: OutBase/source. The
// rev is already in OutBase, so an immutable rev makes this a valid cache.
func sourceCheckout(cfg *Config) string {
	return filepath.Join(outBase(cfg), "source")
}

// fetchSource ensures the pinned source is checked out at dir and returns
// its absolute path. An existing checkout is reused (immutable rev →
// valid; a mutable ref may go stale — pekit clean forces a re-fetch). A
// failed checkout is torn down so the next run re-clones rather than
// reusing a half-built tree.
func fetchSource(src *Source, dir string) (string, error) {
	// Local mode: build the working copy in place, no clone/checkout.
	// LocalPath is relative to the recipe dir (the current directory).
	if src.Local {
		abs, err := filepath.Abs(src.LocalPath)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("localpath %s: %w", src.LocalPath, err)
		}
		fmt.Printf("pekit: source: %s (local)\n", abs)
		return abs, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err == nil {
		return abs, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	fmt.Printf("pekit: source: %s @ %s\n", src.Git, src.Rev)
	if err := runGit("", "clone", "--quiet", src.Git, abs); err != nil {
		os.RemoveAll(abs)
		return "", fmt.Errorf("cloning %s: %w", src.Git, err)
	}
	if err := runGit(abs, "checkout", "--quiet", "--detach", src.Rev); err != nil {
		os.RemoveAll(abs)
		return "", fmt.Errorf("checking out %s: %w", src.Rev, err)
	}
	return abs, nil
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdClean runs the [clean] command if the project defines one, then
// removes outDir. Unlike other verbs a missing [clean] section is fine:
// pekit always knows how to clean the stages it owns.
func cmdClean(args []string, ver *Version) error {
	name, err := targetArg(args)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return err
	}

	targets, hasSection := cfg.Commands["clean"]
	if !hasSection && len(args) == 1 {
		return fmt.Errorf("pekit.toml has no [clean] section")
	}
	if hasSection {
		target, ok := targets[name]
		if !ok {
			return fmt.Errorf("no clean target %q (available: %s)",
				name, strings.Join(sortedNames(targets), ", "))
		}
		if err := runCommandTarget(cfg, "clean", name, target); err != nil {
			return err
		}
	}

	if cfg.OutDir != "" {
		if err := os.RemoveAll(cfg.OutDir); err != nil {
			return fmt.Errorf("removing %s: %w", cfg.OutDir, err)
		}
		fmt.Printf("pekit: removed %s\n", cfg.OutDir)
	}
	return nil
}

// envPrelude compiles [env] into export lines prepended to command
// scripts. Values land verbatim inside double quotes, so sh expansion
// ($HOME, $TC, $(...)) works in them exactly as it does in command.
func envPrelude(env []EnvVar) string {
	var b strings.Builder
	for _, e := range env {
		fmt.Fprintf(&b, "export %s=\"%s\"\n", e.Name, e.Value)
	}
	return b.String()
}

func envNames(env []EnvVar) []string {
	names := make([]string, 0, len(env))
	for _, e := range env {
		names = append(names, e.Name)
	}
	return names
}

func cmdPackage(args []string, ver *Version, local, noBuild bool) error {
	sel, err := packageSelector(args)
	if err != nil {
		return err
	}
	_, _, err = buildPackages(sel, ver, local, noBuild)
	return err
}

// packageSelector pulls the optional package-name argument out of a
// package/publish invocation: none selects every package the recipe defines,
// one names a single package, more is a usage error.
func packageSelector(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("usage: pekit <package|publish> [name]")
	}
}

// packResult is one packaged artifact: the effective package name, the path
// written, and the merged definition it was built from (so publish can ship
// it to the package's own [publish] targets).
type packResult struct {
	Name     string
	Artifact string
	Pkg      *PackageFile
}

// buildContext is the once-per-invocation state every package a recipe emits
// shares: the loaded build config, the fetched source tree, and the merged
// fill-only base table (workspace root < source < the recipe's own unprefixed
// package.pekit.toml). prepareBuild assembles it; packOne consumes it once per
// package, overlaying that package's prefixed file on top of baseRaw.
type buildContext struct {
	cfg           *Config
	wd            string
	wsRoot        string         // workspace root, or "" if not in a workspace
	provenanceDir string         // git tree identifying what built the packages
	literalRoot   string         // root for plain (non-stage) [files] sources
	baseRaw       map[string]any // shared fill-only base; may be nil
	local         bool
	noBuild       bool            // --no-build: reuse staged builds, build only missing
	ran           map[string]bool // build targets already run this invocation
}

// buildPackages prepares the shared build once, then packs each selected
// package: sel names one, or "" means all. Members are the prefixed
// <name>.package.pekit.toml files in the recipe dir; with none present the
// bare package.pekit.toml is the sole package (the original single-package
// behaviour). Returns one result per package (for publish) and the workspace
// root the recipe belongs to ("" if none).
func buildPackages(sel string, ver *Version, local, noBuild bool) ([]packResult, string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	members, err := discoverPackages(wd)
	if err != nil {
		return nil, "", err
	}

	// Resolve the selection to a list of (name, prefixed-table) jobs before
	// doing any build work, so a bad name fails before fetching a source.
	type job struct {
		name string         // prefix selector ("" for the standalone base)
		raw  map[string]any // the prefixed file's table (nil for standalone)
	}
	var jobs []job
	if len(members) == 0 {
		if sel != "" {
			return nil, "", fmt.Errorf("pekit package %q: this recipe defines no named packages (it has a single package.pekit.toml)", sel)
		}
		jobs = []job{{}}
	} else {
		chosen := members
		if sel != "" {
			if !slices.Contains(members, sel) {
				return nil, "", fmt.Errorf("pekit package %q: no such package (available: %s)", sel, strings.Join(members, ", "))
			}
			chosen = []string{sel}
		}
		for _, m := range chosen {
			raw, _, derr := decodePackageFile(m+".package.pekit.toml", ver)
			if derr != nil {
				return nil, "", derr
			}
			jobs = append(jobs, job{name: m, raw: raw})
		}
	}

	bc, err := prepareBuild(wd, ver, local)
	if err != nil {
		return nil, "", err
	}
	bc.noBuild = noBuild
	if len(jobs) > 1 {
		fmt.Fprintf(os.Stderr, "pekit: packaging %d packages: %s\n", len(jobs), strings.Join(members, ", "))
	}

	var results []packResult
	for _, j := range jobs {
		artifact, name, pf, perr := packOne(bc, j.raw, j.name)
		if perr != nil {
			return nil, "", perr
		}
		results = append(results, packResult{Name: name, Artifact: artifact, Pkg: pf})
	}
	return results, bc.wsRoot, nil
}

// prepareBuild assembles the buildContext: load the recipe, apply --local,
// fetch the [source] tree (delegate mode, with section-level fallback), and
// merge the fill-only base package table (workspace root < source < the
// recipe's own package.pekit.toml). The expensive parts — source fetch, and
// later the build steps — happen once and are shared by every package the
// recipe emits.
func prepareBuild(wd string, ver *Version, local bool) (*buildContext, error) {
	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return nil, err
	}
	if err := applyLocal(cfg, local); err != nil {
		return nil, err
	}

	// The recipe's own package.pekit.toml, kept as a raw table so a partial
	// override can be merged field-by-field below (a struct can't tell
	// "field unset" from "field empty"). When prefixed members exist this is
	// their shared base; with none it is the sole package itself.
	recipeRaw, _, err := decodePackageFile("package.pekit.toml", ver)
	if err != nil {
		return nil, err
	}
	_, hasBuild := cfg.Commands["build"]

	// Workspace defaults: if a workspace.pekit.toml sits above this recipe,
	// its sibling package.pekit.toml is the lowest merge layer (fill-only
	// defaults like [publish]). The marker is what gates this — a bare
	// ancestor package.pekit.toml is never inherited.
	wsRoot, inWorkspace, err := findWorkspaceRoot(wd)
	if err != nil {
		return nil, err
	}
	var rootRaw map[string]any
	if inWorkspace {
		if rootRaw, _, err = decodePackageFile(filepath.Join(wsRoot, "package.pekit.toml"), ver); err != nil {
			return nil, err
		}
	} else {
		wsRoot = ""
	}

	// With [source], the recipe and the upstream merge per section: a
	// self-describing upstream (loregd) ships its own pekit files, so a
	// recipe can be just [source] and inherit everything — or override
	// individual pieces. pekit.toml's [build]/[env] fall back whole;
	// package.pekit.toml's [package] merges field-by-field, its other
	// sections whole.
	provenanceDir, literalRoot := wd, wd
	var srcRaw map[string]any
	if cfg.Source != nil {
		// Delegate mode builds from the source and may borrow parts of its
		// recipe, so fetch it up front (the build step reuses the checkout).
		// fetchSource resolves the actual source dir for both modes: the
		// git checkout, or — under --local — the localpath working copy.
		checkout, ferr := fetchSource(cfg.Source, sourceCheckout(cfg))
		if ferr != nil {
			return nil, ferr
		}
		provenanceDir, literalRoot = checkout, checkout

		// pekit.toml: section-level fallback.
		if srcCfg, cerr := LoadConfig(filepath.Join(checkout, "pekit.toml"), ver); cerr == nil {
			if !hasBuild {
				if b, ok := srcCfg.Commands["build"]; ok {
					cfg.Commands["build"] = b
					hasBuild = true
				}
			}
			if len(cfg.Env) == 0 {
				cfg.Env = srcCfg.Env
			}
		}

		if srcRaw, _, err = decodePackageFile(filepath.Join(checkout, "package.pekit.toml"), ver); err != nil {
			return nil, err
		}
	}

	return &buildContext{
		cfg:           cfg,
		wd:            wd,
		wsRoot:        wsRoot,
		provenanceDir: provenanceDir,
		literalRoot:   literalRoot,
		// Precedence (low to high): workspace root < source < recipe base.
		// A package's own prefixed file is overlaid on top of this in packOne.
		baseRaw: mergePackageRaw(recipeRaw, mergePackageRaw(srcRaw, rootRaw)),
		local:   local,
		ran:     map[string]bool{},
	}, nil
}

// packOne packs a single package: overlay its prefixed file (prefixRaw, nil
// for the standalone base) on the shared base, parse, run the build targets
// its [files] imply (each at most once per invocation), then stage and pack.
// selName is the prefix that selected it ("" = the standalone package).
// Returns the artifact path and the effective package name.
func packOne(bc *buildContext, prefixRaw map[string]any, selName string) (string, string, *PackageFile, error) {
	merged := mergePackageRaw(prefixRaw, bc.baseRaw)
	if merged == nil {
		return "", "", nil, fmt.Errorf("no package.pekit.toml found (recipe has none; [source] upstream provides none)")
	}
	pf, err := parsePackageRaw(merged)
	if err != nil {
		return "", "", nil, fmt.Errorf("package.pekit.toml: %w", err)
	}

	// A member's name is its filename prefix — selector and artifact name are
	// one and the same — unless the member file itself sets [package] name (an
	// inherited base name never leaks across members, which would collide
	// them). The standalone package keeps the [package] name / dir default.
	name := selName
	if selName != "" {
		if override := rawPackageName(prefixRaw); override != "" {
			name = override
		}
	} else {
		name = pf.Name
		if name == "" {
			name = defaultName(bc.cfg.Source, bc.wd)
		}
	}

	engine, err := engineFor(pf.Format)
	if err != nil {
		return "", "", nil, fmt.Errorf("package %s: %w", name, err)
	}
	if bc.cfg.OutDir == "" {
		return "", "", nil, fmt.Errorf("package %s: packaging requires outDir in pekit.toml", name)
	}

	// Stage references name the build targets they consume, so packaging can
	// rebuild them itself and never package a stale stage. Run each before
	// packaging, but at most once per invocation — several packages slicing
	// one source tree (glibc → libc, libc-dev, locales) build it once, not
	// once each. Literal paths are underivable and stay the caller's problem.
	for _, targetName := range referencedBuildTargets(pf) {
		if _, ok := bc.cfg.Commands["build"][targetName]; !ok {
			return "", "", nil, fmt.Errorf("package %s: [files] references build target %q but no [build.%s] in recipe or source",
				name, targetName, targetName)
		}
		// runTarget builds the target and its dependencies, each at most once
		// across this invocation (so packages sharing a target — or its deps —
		// build it just once). Under --no-build it reuses whatever is already
		// staged and builds only the targets that are missing.
		if err := runTarget(bc.cfg, "build", targetName, bc.ran, bc.noBuild); err != nil {
			return "", "", nil, err
		}
	}

	files, err := resolveFiles(pf, name, outBase(bc.cfg), bc.literalRoot)
	if err != nil {
		return "", "", nil, err
	}
	outStage, err := prepareOutDir(outBase(bc.cfg), "package", name, bc.cfg.ClearOut)
	if err != nil {
		return "", "", nil, err
	}

	fmt.Printf("pekit: package %s (format %s, %d files)\n", name, pf.Format, len(files))
	artifact, err := engine(PackageJob{Pkg: pf, Name: name, Root: bc.wd, ProvenanceDir: bc.provenanceDir, Files: files, OutStage: outStage, Local: bc.local})
	if err != nil {
		return "", "", nil, err
	}
	fmt.Printf("pekit: wrote %s\n", artifact)
	return artifact, name, pf, nil
}

// discoverPackages returns the recipe's prefixed package members — every
// "<name>.package.pekit.toml" in dir — sorted by name. The bare
// "package.pekit.toml" is the shared base, not a member, so it is excluded.
// Members live only in the recipe dir: a source or workspace-root file
// contributes its unprefixed base to every member, not members of its own.
func discoverPackages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	const suffix = ".package.pekit.toml"
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if prefix, ok := strings.CutSuffix(e.Name(), suffix); ok && prefix != "" {
			names = append(names, prefix)
		}
	}
	sort.Strings(names)
	return names, nil
}

// rawPackageName reads [package] name from an undecoded package table, or ""
// if unset — used to tell a member that overrides its own name from one that
// would merely inherit a name from the shared base.
func rawPackageName(raw map[string]any) string {
	pkg, ok := raw["package"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := pkg["name"].(string)
	return name
}

// cmdPublish builds the selected package(s), then ships each artifact to its
// own [publish] targets (usually inherited from the workspace root). A bare
// `pekit publish` publishes every package the recipe defines.
func cmdPublish(args []string, ver *Version, local, noBuild bool) error {
	sel, err := packageSelector(args)
	if err != nil {
		return err
	}
	results, wsRoot, err := buildPackages(sel, ver, local, noBuild)
	if err != nil {
		return err
	}
	// localdir paths are workspace-root-relative; without a workspace they
	// fall back to the recipe dir.
	base := wsRoot
	if base == "" {
		if base, err = os.Getwd(); err != nil {
			return err
		}
	}
	for _, r := range results {
		if len(r.Pkg.Publish) == 0 {
			return fmt.Errorf("package %s: no [publish] targets (add [[publish.<type>]] to package.pekit.toml or the workspace root)", r.Name)
		}
		for _, t := range r.Pkg.Publish {
			switch t.Type {
			case "localdir":
				if err := publishLocalDir(r.Artifact, filepath.Join(base, t.Path)); err != nil {
					return fmt.Errorf("publish localdir: %w", err)
				}
			default:
				return fmt.Errorf("publish: unsupported target type %q", t.Type)
			}
		}
	}
	return nil
}

// publishLocalDir copies the built artifact into a local repository
// directory, creating it if needed.
func publishLocalDir(artifact, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(destDir, filepath.Base(artifact))
	if err := copyFile(artifact, dest); err != nil {
		return err
	}
	fmt.Printf("pekit: published %s → %s\n", filepath.Base(artifact), destDir)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// defaultName derives a package name when [package] name is unset: the
// source repo (git URL basename minus .git) for a [source] recipe — so
// the name doesn't depend on the checkout dir — else the project dir.
func defaultName(src *Source, wd string) string {
	if src != nil {
		base := strings.TrimSuffix(filepath.Base(strings.TrimRight(src.Git, "/")), ".git")
		if base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return filepath.Base(wd)
}

// referencedBuildTargets returns the distinct build targets to run
// before packaging, sorted: those derived from stage-reference sources
// unioned with the declared builds list (additive — declared targets
// can never suppress a derived one).
func referencedBuildTargets(pf *PackageFile) []string {
	set := make(map[string]bool)
	for _, m := range pf.Files {
		if m.Source.Target != "" {
			set[m.Source.Target] = true
		}
	}
	for _, t := range pf.Builds {
		set[t] = true
	}
	return sortedNames(set)
}

// resolveFiles turns [files] sources into verified absolute paths: stage
// references resolve under outDir/build/<target>/, plain paths against the
// project root. A glob source expands to every matching staged file (see
// expandSource); the result is deduplicated over the final archive paths
// (the authoritative collision check, since glob dests are only known once
// expanded) and sorted by dest for deterministic, reproducible packing.
func resolveFiles(pf *PackageFile, name, outDir, literalRoot string) ([]StagedFile, error) {
	var files []StagedFile
	seen := make(map[string]string) // archive path -> source that produced it
	fired := make([]bool, len(pf.Exclude))
	for _, m := range pf.Files {
		root := literalRoot
		if m.Source.Target != "" {
			root = filepath.Join(outDir, "build", m.Source.Target)
		}
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		staged, err := expandSource(root, m.Source, m.Dest, name)
		if err != nil {
			return nil, err
		}
		for _, sf := range staged {
			// Drop staged files that match an [files].exclude pattern. The
			// match is on the source path (stage-relative), so it lines up
			// with the source-style ":usr/bin/..." patterns even when the
			// mapping rebases its dest elsewhere.
			if len(pf.Exclude) > 0 {
				rel, rerr := filepath.Rel(rootAbs, sf.Source)
				if rerr != nil {
					return nil, rerr
				}
				if i := excludedBy(pf.Exclude, m.Source.Target, filepath.ToSlash(rel)); i >= 0 {
					fired[i] = true
					continue
				}
			}
			if prev, dup := seen[sf.Dest]; dup {
				return nil, fmt.Errorf("package %s: %s and %s both map to %q",
					name, prev, m.Source.String(), sf.Dest)
			}
			seen[sf.Dest] = m.Source.String()
			files = append(files, sf)
		}
	}
	// An exclude that drops nothing is usually a stale or mistyped pattern
	// (e.g. an upstream renamed a tool). Warn rather than fail, so one recipe
	// still builds cleanly across versions with slightly different layouts.
	for i, ex := range pf.Exclude {
		if !fired[i] {
			fmt.Fprintf(os.Stderr, "pekit: warning: package %s: exclude %q matched no staged files\n", name, ex.String())
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Dest < files[j].Dest })
	return files, nil
}
