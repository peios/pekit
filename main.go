package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const usage = "usage: pekit <build|test|install|clean> [target] [--no-build[=t1,...]] [--env name] [--keyring=file] [--keyring.k=v ...] | pekit <package|publish> [name] [--local] [--no-build[=t1,...]] [--env name] [--keyring=file] [--keyring.k=v ...] | pekit workspace <package|publish> <--all|--latest|--local>"

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

	// A remote-recipe argument (github.com/owner/repo, a git URL) clones the
	// recipe and re-runs the verb inside the checkout. Detected on the raw args
	// so the verb and its flags can be re-dispatched unchanged.
	if rest, spec, ok := extractRemoteSpec(args); ok {
		return runRemote(spec, rest)
	}

	args, f, err := extractFlags(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New(usage)
	}

	// --no-build trusts the already-staged build tree, so it means something to
	// every verb that runs build targets — build itself, the stage-and-pack
	// verbs (package/publish), and test/install (their needs are build targets).
	// Only clean never builds anything.
	if f.noBuild.active && args[0] == "clean" {
		return fmt.Errorf("--no-build does not apply to clean")
	}

	// Resolve --keyring=<file> / --keyring.<a.b>=<v> into the injected env map
	// once, from the invocation directory (where the keyring files live).
	kr, err := resolveKeyring(f.keyring)
	if err != nil {
		return err
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
			return dispatch(args, ver, true, f.noBuild, f.env, kr)
		}

		// Working copy not present — fall back to remote instead of failing.
		if src.Git == "" && src.URL == "" {
			return fmt.Errorf("--local: %s and no remote source to fall back to", localMissReason(src))
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
		return dispatch(args, ver, false, f.noBuild, f.env, kr)
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
		return dispatch(args, vers[0], false, f.noBuild, f.env, kr)
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
		if err := dispatch(args, ver, false, f.noBuild, f.env, kr); err != nil {
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

func dispatch(args []string, ver *Version, local bool, noBuild noBuildSet, env string, keyring map[string]string) error {
	switch args[0] {
	case "build", "test", "install":
		return cmdVerb(args[0], args[1:], ver, local, noBuild, env, keyring)
	case "package":
		return cmdPackage(args[1:], ver, local, noBuild, env, keyring)
	case "publish":
		return cmdPublish(args[1:], ver, local, noBuild, env, keyring)
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
	local      bool       // --local: build the [source] localpath working copy
	noBuild    noBuildSet // --no-build[=t1,...]: reuse already-staged builds
	env        string      // --env [main|none|<name>]: command-wrap selection (default main)
	keyring    keyringSpec // --keyring=<file> and --keyring.<a.b>=<value>
}

// noBuildSet records which build targets --no-build should reuse if they are
// already staged, instead of rebuilding them:
//
//	active=false            --no-build absent: rebuild everything.
//	active=true, all=true    bare --no-build: reuse every staged target.
//	active=true, all=false   --no-build=a,b: reuse only the named targets,
//	                         rebuild the rest.
//
// A target is only ever reused when its stage dir already exists; a selected-
// but-unstaged target is still built, mirroring bare --no-build's "build only
// what is missing" rule.
type noBuildSet struct {
	active bool
	all    bool
	names  map[string]bool
}

// skip reports whether target should be reused-if-staged rather than rebuilt.
func (n noBuildSet) skip(target string) bool {
	return n.active && (n.all || n.names[target])
}

// parseNoBuild parses the value of --no-build=a,b,c into a target selection.
// An empty value (--no-build=) is rejected: bare --no-build reuses everything.
func parseNoBuild(val string) (noBuildSet, error) {
	names := map[string]bool{}
	for _, part := range strings.Split(val, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return noBuildSet{}, fmt.Errorf("--no-build=: empty target name (use bare --no-build to reuse all staged builds)")
		}
		names[name] = true
	}
	return noBuildSet{active: true, names: names}, nil
}

// extractFlags pulls the global flags out of args, returning the rest.
func extractFlags(args []string) ([]string, flags, error) {
	var rest []string
	f := flags{env: "main"} // --env defaults to main (apply env.pekit.toml if present)
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
			f.noBuild = noBuildSet{active: true, all: true}
		case strings.HasPrefix(a, "--no-build="):
			set, perr := parseNoBuild(strings.TrimPrefix(a, "--no-build="))
			if perr != nil {
				return nil, f, perr
			}
			f.noBuild = set
		case a == "--env":
			if i+1 >= len(args) {
				return nil, f, fmt.Errorf("%s requires a value", a)
			}
			f.env = args[i+1]
			i++
		case strings.HasPrefix(a, "--env="):
			f.env = strings.TrimPrefix(a, "--env=")
		case strings.HasPrefix(a, "--keyring="):
			// --keyring=<name> loads <name>.keyring.pekit.toml beside the recipe.
			name := strings.TrimPrefix(a, "--keyring=")
			if name == "" {
				return nil, f, fmt.Errorf("--keyring= requires a name (--keyring=<file>)")
			}
			f.keyring.files = append(f.keyring.files, name)
		case strings.HasPrefix(a, "--keyring."):
			// --keyring.<dotted.name>=<value> → exported as PEKIT_KEYRING_<NAME>
			// (uppercased, non-alphanumerics to '_'). The value is opaque.
			rest := strings.TrimPrefix(a, "--keyring.")
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return nil, f, fmt.Errorf("%s requires a value (--keyring.<name>=<value>)", a)
			}
			path, value := rest[:eq], rest[eq+1:]
			if path == "" {
				return nil, f, fmt.Errorf("--keyring. requires a name (--keyring.<name>=<value>)")
			}
			if f.keyring.vars == nil {
				f.keyring.vars = map[string]string{}
			}
			f.keyring.vars["PEKIT_KEYRING_"+envTargetName(path)] = value
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

func cmdVerb(verb string, args []string, ver *Version, local bool, noBuild noBuildSet, env string, keyring map[string]string) error {
	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return err
	}
	if err := applyLocal(cfg, local); err != nil {
		return err
	}
	if err := applyEnv(cfg, env); err != nil {
		return err
	}
	cfg.Keyring = keyring

	targets, ok := cfg.Commands[verb]
	if !ok {
		return fmt.Errorf("pekit.toml has no [%s] section", verb)
	}
	names, err := verbTargets(verb, args, targets)
	if err != nil {
		return err
	}
	if len(names) > 1 {
		fmt.Fprintf(os.Stderr, "pekit: %s: %d targets: %s\n", verb, len(names), strings.Join(names, ", "))
	}
	// Shared ran set: a dependency several targets declare via needs builds
	// once across the whole fan-out, not once per target.
	ran := map[string]bool{}
	for _, name := range names {
		if err := runTarget(cfg, verb, name, ran, noBuild); err != nil {
			return err
		}
	}
	return nil
}

// verbTargets resolves which targets a build/test/install invocation runs. A
// named argument selects exactly that target. With no argument it is the bare
// "main" target when the section has one (the single-[verb] form), otherwise
// every named target in sorted order — so a recipe with only [install.cli] and
// [install.server] installs both on a bare `pekit install`, mirroring how a
// bare `pekit package` builds every package.
func verbTargets(verb string, args []string, targets map[string]Target) ([]string, error) {
	switch len(args) {
	case 0:
		if _, ok := targets["main"]; ok {
			return []string{"main"}, nil
		}
		return sortedNames(targets), nil
	case 1:
		if _, ok := targets[args[0]]; !ok {
			return nil, fmt.Errorf("no %s target %q (available: %s)",
				verb, args[0], strings.Join(sortedNames(targets), ", "))
		}
		return []string{args[0]}, nil
	default:
		return nil, errors.New(usage)
	}
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

// runTarget runs target `name` in section `verb`. A target's dependencies
// (`needs`) are always BUILD targets, regardless of `verb`: a build target's
// build subgraph is run in order; a test/install/clean target first has its
// needed build targets staged, then runs its own command with each direct
// need exposed as PEKIT_<NAME>_OUT. ran tracks build targets already handled
// this invocation so a build shared by several targets runs once; pass nil for
// a one-shot run.
//
// When noBuild selects a build target (bare --no-build selects all), a target
// whose stage dir already exists is reused instead of rebuilt; one that was
// never built is still built. --no-build names always refer to build targets.
func runTarget(cfg *Config, verb, name string, ran map[string]bool, noBuild noBuildSet) error {
	if err := validateNoBuildNames(cfg, noBuild); err != nil {
		return err
	}
	if verb == "build" {
		return runBuildSubgraph(cfg, name, ran, noBuild)
	}
	// test/install/clean: stage the build targets this one needs, then run it.
	target := cfg.Commands[verb][name]
	for _, dep := range target.Needs {
		if err := runBuildSubgraph(cfg, dep, ran, noBuild); err != nil {
			return err
		}
	}
	return runCommandTarget(cfg, verb, name, target)
}

// validateNoBuildNames rejects a --no-build=names selection that names anything
// that isn't a build target, so a typo surfaces instead of silently rebuilding.
// (--no-build always refers to build targets, since builds are the only staged,
// reusable outputs.)
func validateNoBuildNames(cfg *Config, noBuild noBuildSet) error {
	if !noBuild.active || noBuild.all {
		return nil
	}
	build := cfg.Commands["build"]
	var missing []string
	for n := range noBuild.names {
		if _, ok := build[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("--no-build: no build target %s (available: %s)",
			strings.Join(missing, ", "), strings.Join(sortedNames(build), ", "))
	}
	return nil
}

// runBuildSubgraph runs build target `name` and its transitive build needs in
// dependency order, honoring the ran-dedup and --no-build reuse rules.
func runBuildSubgraph(cfg *Config, name string, ran map[string]bool, noBuild noBuildSet) error {
	targets := cfg.Commands["build"]
	// Walk top-down so a reused target prunes its dependency subtree: a target's
	// `needs` exist only to build it, so if it is reused (--no-build + already
	// staged) there is no reason to build its dependencies. We only descend into
	// the needs of a target we are actually going to build. The build graph is
	// validated acyclic at parse time, so this recursion always terminates.
	var ensure func(string) error
	ensure = func(t string) error {
		if ran[t] {
			return nil
		}
		if noBuild.skip(t) {
			if _, err := os.Stat(filepath.Join(outBase(cfg), "build", t)); err == nil {
				fmt.Printf("pekit: build %s: reusing staged output (--no-build)\n", t)
				if ran != nil {
					ran[t] = true
				}
				return nil // reused → prune its dependency subtree
			}
		}
		// Building t: its dependencies must be staged first.
		for _, dep := range targets[t].Needs {
			if err := ensure(dep); err != nil {
				return err
			}
		}
		if err := runCommandTarget(cfg, "build", t, targets[t]); err != nil {
			return err
		}
		if ran != nil {
			ran[t] = true
		}
		return nil
	}
	return ensure(name)
}

func runCommandTarget(cfg *Config, verb, name string, target Target) error {
	// Build a SELF-CONTAINED script: the PEKIT_* and [env] values are baked in
	// as `export` lines (not just process env) so they survive into a wrapped
	// environment — a docker container or a nix-shell --pure scrubs the inherited
	// env, but exports inside the script text always take effect. The same
	// script then runs directly (unwrapped) or quoted inside cfg.Wrap.
	var b strings.Builder
	var workdir, stageDir string

	// Source checkout + PEKIT_OUT staging are build-only.
	if verb == "build" && cfg.OutDir != "" {
		if cfg.Source != nil {
			srcDir, err := fetchSource(cfg.Source, sourceCheckout(cfg))
			if err != nil {
				return err
			}
			workdir = srcDir
		}
		var err error
		stageDir, err = prepareOutDir(outBase(cfg), verb, name, cfg.ClearOut)
		if err != nil {
			return err
		}
		fmt.Printf("pekit: out: %s\n", stageDir)
		fmt.Fprintf(&b, "export PEKIT_OUT=%s\n", shellQuote(stageDir))
	}

	// Dependencies are always build targets; expose each direct need's staged
	// output dir as PEKIT_<NAME>_OUT — in every section, so a test or install
	// command can consume what it was built against. runTarget staged them
	// first (under out/.../build/<dep>), so the dirs exist.
	for _, dep := range target.Needs {
		depStage, derr := filepath.Abs(filepath.Join(outBase(cfg), "build", dep))
		if derr != nil {
			return derr
		}
		fmt.Fprintf(&b, "export PEKIT_%s_OUT=%s\n", envTargetName(dep), shellQuote(depStage))
	}
	if len(target.Needs) > 0 {
		fmt.Printf("pekit: needs: %s\n", strings.Join(target.Needs, ", "))
	}

	// Injected keyring values (--keyring.<name>=<value>) are exported like
	// PEKIT_OUT — baked in so they survive a wrapped environment. The names are
	// logged; the values are opaque and never printed.
	if len(cfg.Keyring) > 0 {
		names := make([]string, 0, len(cfg.Keyring))
		for k := range cfg.Keyring {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(cfg.Keyring[k]))
		}
		fmt.Printf("pekit: keyring: %s\n", strings.Join(names, ", "))
	}

	if len(cfg.Env) > 0 {
		b.WriteString(envPrelude(cfg.Env))
		fmt.Printf("pekit: env: %s\n", strings.Join(envNames(cfg.Env), ", "))
	}
	b.WriteString(target.Command)
	inner := b.String()

	fmt.Printf("pekit: %s %s: %s\n", verb, name, target.Command)

	// Wrap: substitute the self-contained script (shell-quoted as one argument)
	// into the env's [wrap] template; without a wrap, run the script directly.
	script := inner
	if cfg.Wrap != "" {
		script = strings.ReplaceAll(cfg.Wrap, "{{command}}", shellQuote(inner))
		fmt.Printf("pekit: wrap: %s\n", cfg.Wrap)
	}

	// -eu so multi-line commands stop at the first failure instead of
	// barrelling on (e.g. staging a stale binary after a failed compile).
	cmd := exec.Command("sh", "-euc", script)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if workdir != "" {
		cmd.Dir = workdir
	}

	if err := cmd.Run(); err != nil {
		return err
	}
	if stageDir != "" && isEmptyDir(stageDir) {
		fmt.Fprintf(os.Stderr, "pekit: warning: build %s left %s empty\n", name, stageDir)
	}
	return nil
}

// shellQuote wraps s in single quotes for POSIX sh, escaping embedded single
// quotes via the '\'' idiom — so an arbitrary (possibly multi-line) script can
// be passed as a single argument to a wrapper like `nix-shell --run` or
// `docker run … sh -euc`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// revScope is the filesystem-safe form of a source rev ('/' flattened).
// Local mode has no rev, so its builds share one "localdev" scope.
func revScope(src *Source) string {
	if src.Local {
		return "localdev"
	}
	if src.URL != "" {
		return urlScope(src.URL)
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
// failed checkout is torn down so the next run re-fetches rather than
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
	// URL mode: download (and optionally extract) the release archive rather
	// than cloning git.
	if src.URL != "" {
		return fetchURL(src, dir)
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
	// Shallow first: fetch only the pinned rev at depth 1, so for the tag/branch
	// revs every recipe uses we transfer a single commit instead of the whole
	// upstream history (GCC's is multi-GB). A bare-SHA rev a server won't serve
	// shallowly (no uploadpack.allowReachableSHA1InWant) falls back to a full
	// clone.
	if err := shallowFetch(src, abs); err != nil {
		os.RemoveAll(abs)
		if cerr := fullClone(src, abs); cerr != nil {
			os.RemoveAll(abs)
			return "", cerr
		}
	}
	return abs, nil
}

// shallowFetch makes a depth-1 checkout of src.Rev in abs (which must not yet
// exist). It resolves any ref a server serves by name — tags and branches, the
// only rev forms our recipes use — transferring a single commit instead of the
// whole history. A rev the server only serves as a reachable SHA fails here, and
// the caller falls back to fullClone.
func shallowFetch(src *Source, abs string) error {
	if err := runGit("", "init", "--quiet", abs); err != nil {
		return fmt.Errorf("git init %s: %w", abs, err)
	}
	if err := runGit(abs, "remote", "add", "origin", src.Git); err != nil {
		return fmt.Errorf("adding remote %s: %w", src.Git, err)
	}
	if err := runGit(abs, "fetch", "--quiet", "--depth", "1", "origin", src.Rev); err != nil {
		return fmt.Errorf("shallow-fetching %s @ %s: %w", src.Git, src.Rev, err)
	}
	if err := runGit(abs, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checking out %s: %w", src.Rev, err)
	}
	return nil
}

// fullClone is the fallback for revs that can't be fetched shallowly: clone the
// whole repository into abs, then check the rev out by name.
func fullClone(src *Source, abs string) error {
	if err := runGit("", "clone", "--quiet", src.Git, abs); err != nil {
		return fmt.Errorf("cloning %s: %w", src.Git, err)
	}
	if err := runGit(abs, "checkout", "--quiet", "--detach", src.Rev); err != nil {
		return fmt.Errorf("checking out %s: %w", src.Rev, err)
	}
	return nil
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

func cmdPackage(args []string, ver *Version, local bool, noBuild noBuildSet, env string, keyring map[string]string) error {
	sel, err := packageSelector(args)
	if err != nil {
		return err
	}
	_, _, err = buildPackages(sel, ver, local, noBuild, env, keyring)
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
	noBuild       noBuildSet      // --no-build[=t1,...]: reuse staged builds
	ran           map[string]bool // build targets already run this invocation
}

// buildPackages prepares the shared build once, then packs each selected
// package: sel names one, or "" means all. Members are the prefixed
// <name>.package.pekit.toml files in the recipe dir; with none present the
// bare package.pekit.toml is the sole package (the original single-package
// behaviour). Returns one result per package (for publish) and the workspace
// root the recipe belongs to ("" if none).
func buildPackages(sel string, ver *Version, local bool, noBuild noBuildSet, env string, keyring map[string]string) ([]packResult, string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	// prepareBuild fetches the [source] tree (delegate mode) and resolves the
	// shared base, so it runs before member discovery: a delegate recipe's
	// members live in the fetched source, discoverable only once it is checked
	// out. (Without a [source] the fetch is a no-op, so nothing is paid here.)
	bc, err := prepareBuild(wd, ver, local, env, keyring)
	if err != nil {
		return nil, "", err
	}
	bc.noBuild = noBuild

	// Member files come from the recipe dir and, in delegate mode, the fetched
	// source tree — discovered there exactly as a normal run would. A name in
	// both merges recipe over source (mirroring the base's per-section
	// delegation); a name in only one is taken whole.
	recipePath := map[string]string{} // member name -> recipe-dir file
	srcPath := map[string]string{}    // member name -> source-tree file
	var order []string
	record := func(ms []packageMember, into map[string]string) {
		for _, m := range ms {
			if _, dup := recipePath[m.name]; !dup {
				if _, dup := srcPath[m.name]; !dup {
					order = append(order, m.name)
				}
			}
			into[m.name] = m.path
		}
	}
	recipeMembers, err := discoverPackages(wd)
	if err != nil {
		return nil, "", err
	}
	record(recipeMembers, recipePath)
	if bc.cfg.Source != nil {
		srcMembers, serr := discoverPackages(bc.literalRoot)
		if serr != nil {
			return nil, "", serr
		}
		record(srcMembers, srcPath)
	}
	sort.Strings(order)

	// Resolve the selection to a list of (name, merged-table) jobs.
	type job struct {
		name string         // prefix selector ("" for the standalone base)
		raw  map[string]any // the merged member table (nil for standalone)
	}
	var jobs []job
	if len(order) == 0 {
		if sel != "" {
			return nil, "", fmt.Errorf("pekit package %q: this recipe defines no named packages (it has a single package.pekit.toml)", sel)
		}
		jobs = []job{{}}
	} else {
		chosen := order
		if sel != "" {
			if _, ok := recipePath[sel]; !ok {
				if _, ok := srcPath[sel]; !ok {
					return nil, "", fmt.Errorf("pekit package %q: no such package (available: %s)", sel, strings.Join(order, ", "))
				}
			}
			chosen = []string{sel}
		}
		for _, name := range chosen {
			raw, derr := decodeMemberFile(recipePath[name], srcPath[name], ver)
			if derr != nil {
				return nil, "", derr
			}
			jobs = append(jobs, job{name: name, raw: raw})
		}
	}

	if len(jobs) > 1 {
		fmt.Fprintf(os.Stderr, "pekit: packaging %d packages: %s\n", len(jobs), strings.Join(order, ", "))
	}

	var results []packResult
	for _, j := range jobs {
		sub, perr := packOne(bc, j.raw, j.name)
		if perr != nil {
			return nil, "", perr
		}
		results = append(results, sub...)
	}
	return results, bc.wsRoot, nil
}

// prepareBuild assembles the buildContext: load the recipe, apply --local,
// fetch the [source] tree (delegate mode, with section-level fallback), and
// merge the fill-only base package table (workspace root < source < the
// recipe's own package.pekit.toml). The expensive parts — source fetch, and
// later the build steps — happen once and are shared by every package the
// recipe emits.
func prepareBuild(wd string, ver *Version, local bool, env string, keyring map[string]string) (*buildContext, error) {
	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return nil, err
	}
	if err := applyLocal(cfg, local); err != nil {
		return nil, err
	}
	cfg.Keyring = keyring
	// The env wrap is resolved at the end, once the source tree is fetched, so
	// it can fall back to the source like [build]/[env] do.

	// The recipe's own package.pekit.toml, kept as a raw table so a partial
	// override can be merged field-by-field below (a struct can't tell
	// "field unset" from "field empty"). When prefixed members exist this is
	// their shared base; with none it is the sole package itself.
	recipeRaw, _, err := decodePackageFile(recipePackageFile(wd), ver)
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
		// Literal [files] sources resolve inside the fetched tree. Provenance
		// anchors to the upstream git tree for a git source, but a url source
		// has no upstream commit — anchor it to the recipe dir (its own repo,
		// if committed) instead of the non-git extracted tarball.
		literalRoot = checkout
		provenanceDir = checkout
		if cfg.Source.URL != "" {
			provenanceDir = wd
		}

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

		if srcRaw, _, err = decodePackageFile(recipePackageFile(checkout), ver); err != nil {
			return nil, err
		}
	}

	// Resolve the env wrap: recipe dir first, then the fetched source tree in
	// delegate mode (literalRoot is the checkout there), so a self-describing
	// source can carry its own build environment while the recipe still wins.
	envDirs := []string{wd}
	if cfg.Source != nil {
		envDirs = append(envDirs, literalRoot)
	}
	if err := applyEnv(cfg, env, envDirs...); err != nil {
		return nil, err
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

// packOne packs the package(s) one job produces: overlay its prefixed file
// (prefixRaw, nil for the standalone base) on the shared base, then pack. A
// plain file yields one result; a [multipack] file fans out into one result
// per enum value (see expandMultipack). selName is the prefix that selected
// the job ("" = the standalone package).
func packOne(bc *buildContext, prefixRaw map[string]any, selName string) ([]packResult, error) {
	merged := mergePackageRaw(prefixRaw, bc.baseRaw)
	if merged == nil {
		return nil, fmt.Errorf("no package.pekit.toml found (recipe has none; [source] upstream provides none)")
	}

	mp, err := parseMultipack(merged)
	if err != nil {
		return nil, err
	}
	if mp != nil {
		return expandMultipack(bc, merged, mp, prefixRaw, selName)
	}

	// No [multipack]: a stray {{multipack}} would survive into a path, so
	// catch it rather than ship the literal placeholder.
	if containsMultipackVar(merged) {
		return nil, fmt.Errorf("package.pekit.toml uses {{multipack}} but defines no [multipack] section")
	}
	pf, err := parsePackageRaw(merged)
	if err != nil {
		return nil, fmt.Errorf("package.pekit.toml: %w", err)
	}
	name := resolveName(bc, pf, prefixRaw, selName, "")
	res, err := packInstance(bc, pf, name)
	if err != nil {
		return nil, err
	}
	return []packResult{res}, nil
}

// resolveMultipackValues yields the enum's values. A literal enum returns them
// as-is; a derived enum.files enum builds its source target (so the stage to
// glob exists — once, via bc.ran, shared with packaging) and enumerates it.
func resolveMultipackValues(bc *buildContext, mp *Multipack) ([]string, error) {
	if mp.Files == nil {
		return mp.Values, nil
	}
	src := mp.Files.Source
	root := bc.literalRoot
	if src.Target != "" {
		if _, ok := bc.cfg.Commands["build"][src.Target]; !ok {
			return nil, fmt.Errorf("[multipack]: enum.files path references build target %q but no [build.%s] in recipe or source",
				src.Target, src.Target)
		}
		if err := runTarget(bc.cfg, "build", src.Target, bc.ran, bc.noBuild); err != nil {
			return nil, err
		}
		root = filepath.Join(outBase(bc.cfg), "build", src.Target)
	}
	values, err := enumerateMultipackValues(root, src, mp.Files.Regex)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "pekit: multipack: %s → %d values: %s\n", src, len(values), strings.Join(values, ", "))
	return values, nil
}

// expandMultipack renders one package instance per enum value: bind
// {{multipack}}, drop the [multipack] directive, parse, and name it (base + the
// rendered suffix). Names must be distinct across the enum, so a suffix (or a
// {{multipack}}-bearing [package] name) is required to keep the packages apart.
// Rendering, parsing, and the distinctness check all run up front, so a bad
// recipe fails before any build work; the build targets the instances share
// then run once, via bc.ran.
func expandMultipack(bc *buildContext, merged map[string]any, mp *Multipack, prefixRaw map[string]any, selName string) ([]packResult, error) {
	values, err := resolveMultipackValues(bc, mp)
	if err != nil {
		return nil, err
	}
	base := withoutKey(merged, "multipack")

	type instance struct {
		pf   *PackageFile
		name string
	}
	insts := make([]instance, 0, len(values))
	byName := make(map[string]string) // package name -> the enum value that produced it
	for _, val := range values {
		rendered, err := renderMultipackValue(base, val)
		if err != nil {
			return nil, err
		}
		pf, err := parsePackageRaw(rendered.(map[string]any))
		if err != nil {
			return nil, fmt.Errorf("package.pekit.toml (multipack %q): %w", val, err)
		}
		name := resolveName(bc, pf, prefixRaw, selName, substituteMultipack(mp.Suffix, val))
		if prev, dup := byName[name]; dup {
			return nil, fmt.Errorf("[multipack]: values %q and %q both produce package name %q; give a suffix that varies with {{multipack}}", prev, val, name)
		}
		byName[name] = val
		insts = append(insts, instance{pf: pf, name: name})
	}

	results := make([]packResult, 0, len(insts))
	for _, in := range insts {
		res, err := packInstance(bc, in.pf, in.name)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// resolveName computes a package's name before suffixing. A member's name is
// its filename prefix — selector and artifact name are one — unless the member
// file itself sets [package] name (an inherited base name never leaks across
// members, which would collide them). The standalone package keeps the
// [package] name / dir default. suffix (multipack's, else "") is appended.
func resolveName(bc *buildContext, pf *PackageFile, prefixRaw map[string]any, selName, suffix string) string {
	base := selName
	if selName != "" {
		if rawPackageName(prefixRaw) != "" {
			base = pf.Name // member overrides its own name (already rendered)
		}
	} else {
		base = pf.Name
		if base == "" {
			base = defaultName(bc.cfg.Source, bc.wd)
		}
	}
	return base + suffix
}

// packInstance runs the build targets one parsed package implies (each at most
// once per invocation — several packages slicing one source tree build it
// once, not once each), then stages and packs it. Literal-path sources are
// underivable and stay the caller's problem.
func packInstance(bc *buildContext, pf *PackageFile, name string) (packResult, error) {
	engine, err := engineFor(pf.Format)
	if err != nil {
		return packResult{}, fmt.Errorf("package %s: %w", name, err)
	}
	if bc.cfg.OutDir == "" {
		return packResult{}, fmt.Errorf("package %s: packaging requires outDir in pekit.toml", name)
	}

	for _, targetName := range referencedBuildTargets(pf) {
		if _, ok := bc.cfg.Commands["build"][targetName]; !ok {
			return packResult{}, fmt.Errorf("package %s: [files] references build target %q but no [build.%s] in recipe or source",
				name, targetName, targetName)
		}
		// runTarget builds the target and its dependencies, each at most once
		// across this invocation (so packages sharing a target — or its deps —
		// build it just once). Under --no-build it reuses whatever is already
		// staged and builds only the targets that are missing.
		if err := runTarget(bc.cfg, "build", targetName, bc.ran, bc.noBuild); err != nil {
			return packResult{}, err
		}
	}

	files, err := resolveFiles(pf, name, outBase(bc.cfg), bc.literalRoot)
	if err != nil {
		return packResult{}, err
	}
	outStage, err := prepareOutDir(outBase(bc.cfg), "package", name, bc.cfg.ClearOut)
	if err != nil {
		return packResult{}, err
	}

	fmt.Printf("pekit: package %s (format %s, %d files)\n", name, pf.Format, len(files))
	artifact, err := engine(PackageJob{Pkg: pf, Name: name, Root: bc.wd, ProvenanceDir: bc.provenanceDir, Files: files, OutStage: outStage, Local: bc.local})
	if err != nil {
		return packResult{}, err
	}
	fmt.Printf("pekit: wrote %s\n", artifact)
	return packResult{Name: name, Artifact: artifact, Pkg: pf}, nil
}

// packageDirs are optional subdirectories beside the recipe that may also hold
// <name>.package.pekit.toml members, so a recipe with many packages can keep
// them out of the recipe root.
var packageDirs = []string{"package.pekit", "packages.pekit"}

const packageSuffix = ".package.pekit.toml"

// packageMember is one discovered prefixed package: its selector name and the
// file it lives in.
type packageMember struct {
	name string
	path string
}

// discoverPackages returns the recipe's prefixed package members — every
// "<name>.package.pekit.toml" in the recipe dir and in the optional
// package.pekit/ or packages.pekit/ subdirectories — sorted by name. The bare
// "package.pekit.toml" is the shared base, not a member, so it is excluded. A
// name found in more than one location is an error. Members live only here: a
// source or workspace-root file contributes its unprefixed base to every
// member, not members of its own.
func discoverPackages(dir string) ([]packageMember, error) {
	seen := map[string]string{} // name -> path, for collision detection
	var members []packageMember
	scan := func(d string) error {
		entries, err := os.ReadDir(d)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // an absent package(s).pekit dir is fine
			}
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			prefix, ok := strings.CutSuffix(e.Name(), packageSuffix)
			if !ok || prefix == "" {
				continue
			}
			p := filepath.Join(d, e.Name())
			if prev, dup := seen[prefix]; dup {
				return fmt.Errorf("duplicate package %q: %s and %s", prefix, prev, p)
			}
			seen[prefix] = p
			members = append(members, packageMember{name: prefix, path: p})
		}
		return nil
	}
	if err := scan(dir); err != nil {
		return nil, err
	}
	for _, sub := range packageDirs {
		if err := scan(filepath.Join(dir, sub)); err != nil {
			return nil, err
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	return members, nil
}

// recipePackageFile returns the path to the recipe's bare package.pekit.toml —
// the shared base / standalone package. It prefers the recipe root, then falls
// back to a package.pekit/ or packages.pekit/ subdirectory, so a recipe can
// keep its base file there alongside its members. When none exists it returns
// the root path (which decodePackageFile treats as "absent"), leaving the
// no-base behaviour unchanged.
func recipePackageFile(dir string) string {
	root := filepath.Join(dir, "package.pekit.toml")
	candidates := []string{root}
	for _, sub := range packageDirs {
		candidates = append(candidates, filepath.Join(dir, sub, "package.pekit.toml"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return root
}

// decodeMemberFile decodes a member's package file, merging a recipe-dir copy
// over a source-tree copy (delegate mode) when both exist — recipe wins,
// per-section, like the shared base. Either path may be empty.
func decodeMemberFile(recipePath, srcPath string, ver *Version) (map[string]any, error) {
	var recipeRaw, srcRaw map[string]any
	if srcPath != "" {
		var err error
		if srcRaw, _, err = decodePackageFile(srcPath, ver); err != nil {
			return nil, err
		}
	}
	if recipePath != "" {
		var err error
		if recipeRaw, _, err = decodePackageFile(recipePath, ver); err != nil {
			return nil, err
		}
	}
	return mergePackageRaw(recipeRaw, srcRaw), nil
}

// memberNames is the selector names of discovered members, in order.
func memberNames(members []packageMember) []string {
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.name
	}
	return names
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
func cmdPublish(args []string, ver *Version, local bool, noBuild noBuildSet, env string, keyring map[string]string) error {
	sel, err := packageSelector(args)
	if err != nil {
		return err
	}
	results, wsRoot, err := buildPackages(sel, ver, local, noBuild, env, keyring)
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
