package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = "usage: pekit <build|test|install|clean> [target] | pekit <package|publish> [--local] | pekit workspace <package|publish> <--all|--latest|--local>"

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

	// Local mode: build the [source] working copy. Source selection
	// (--local) and version numbering (--version) are independent — so
	// --version, if given, only restamps the artifact and never drags the
	// build back to git. The version defaults to a sentinel that sorts
	// below every release; the ledger is off (dev builds aren't recorded).
	if f.local {
		if f.remember {
			return fmt.Errorf("--remember-built does not apply to --local (dev builds aren't recorded)")
		}
		ver := localVersion()
		if f.hasVersion {
			v, perr := parseVersion(f.version)
			if perr != nil {
				return fmt.Errorf("with --local, --version must be an exact version: %w", perr)
			}
			ver = v
		}
		return dispatch(args, ver, true)
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
		return dispatch(args, vers[0], false)
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
		if err := dispatch(args, ver, false); err != nil {
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

func dispatch(args []string, ver *Version, local bool) error {
	switch args[0] {
	case "build", "test", "install":
		return cmdVerb(args[0], args[1:], ver, local)
	case "package":
		return cmdPackage(args[1:], ver, local)
	case "publish":
		return cmdPublish(args[1:], ver, local)
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
		default:
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
	target, ok := targets[name]
	if !ok {
		return fmt.Errorf("no %s target %q (available: %s)",
			verb, name, strings.Join(sortedNames(targets), ", "))
	}
	return runCommandTarget(cfg, verb, name, target)
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

func cmdPackage(args []string, ver *Version, local bool) error {
	if len(args) != 0 {
		return fmt.Errorf("pekit package takes no arguments (one package.pekit.toml per package)")
	}
	_, _, _, err := buildPackage(ver, local)
	return err
}

// buildPackage runs the full package flow for one recipe in the current
// directory and returns the written artifact, the effective (merged)
// package definition, and the workspace root it belongs to ("" if none).
// cmdPackage discards the extras; cmdPublish ships the artifact to the
// package's [publish] targets.
func buildPackage(ver *Version, local bool) (string, *PackageFile, string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", nil, "", err
	}
	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return "", nil, "", err
	}
	if err := applyLocal(cfg, local); err != nil {
		return "", nil, "", err
	}

	// The recipe's own package.pekit.toml, kept as a raw table so a partial
	// override can be merged field-by-field below (a struct can't tell
	// "field unset" from "field empty").
	recipeRaw, _, err := decodePackageFile("package.pekit.toml", ver)
	if err != nil {
		return "", nil, "", err
	}
	_, hasBuild := cfg.Commands["build"]

	// Workspace defaults: if a workspace.pekit.toml sits above this recipe,
	// its sibling package.pekit.toml is the lowest merge layer (fill-only
	// defaults like [publish]). The marker is what gates this — a bare
	// ancestor package.pekit.toml is never inherited.
	wsRoot, inWorkspace, err := findWorkspaceRoot(wd)
	if err != nil {
		return "", nil, "", err
	}
	var rootRaw map[string]any
	if inWorkspace {
		if rootRaw, _, err = decodePackageFile(filepath.Join(wsRoot, "package.pekit.toml"), ver); err != nil {
			return "", nil, "", err
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
			return "", nil, "", ferr
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
			return "", nil, "", err
		}
	}

	// Precedence (low to high): workspace root < source < leaf recipe.
	merged := mergePackageRaw(recipeRaw, mergePackageRaw(srcRaw, rootRaw))
	if merged == nil {
		return "", nil, wsRoot, fmt.Errorf("no package.pekit.toml found (recipe has none; [source] upstream provides none)")
	}
	pf, err := parsePackageRaw(merged)
	if err != nil {
		return "", nil, wsRoot, fmt.Errorf("package.pekit.toml: %w", err)
	}

	name := pf.Name
	if name == "" {
		name = defaultName(cfg.Source, wd)
	}

	engine, err := engineFor(pf.Format)
	if err != nil {
		return "", nil, wsRoot, fmt.Errorf("package %s: %w", name, err)
	}
	if cfg.OutDir == "" {
		return "", nil, wsRoot, fmt.Errorf("package %s: packaging requires outDir in pekit.toml", name)
	}

	// Stage references name the build targets they consume, so packaging
	// can rebuild them itself and never package a stale stage. Literal
	// paths are underivable and stay the caller's freshness problem.
	for _, targetName := range referencedBuildTargets(pf) {
		target, ok := cfg.Commands["build"][targetName]
		if !ok {
			return "", nil, wsRoot, fmt.Errorf("package %s: [files] references build target %q but no [build.%s] in recipe or source",
				name, targetName, targetName)
		}
		if err := runCommandTarget(cfg, "build", targetName, target); err != nil {
			return "", nil, wsRoot, err
		}
	}

	files, err := resolveFiles(pf, name, outBase(cfg), literalRoot)
	if err != nil {
		return "", nil, wsRoot, err
	}
	outStage, err := prepareOutDir(outBase(cfg), "package", name, cfg.ClearOut)
	if err != nil {
		return "", nil, wsRoot, err
	}

	fmt.Printf("pekit: package %s (format %s, %d files)\n", name, pf.Format, len(files))
	artifact, err := engine(PackageJob{Pkg: pf, Name: name, Root: wd, ProvenanceDir: provenanceDir, Files: files, OutStage: outStage, Local: local})
	if err != nil {
		return "", nil, wsRoot, err
	}
	fmt.Printf("pekit: wrote %s\n", artifact)
	return artifact, pf, wsRoot, nil
}

// cmdPublish builds the package, then ships the artifact to each of its
// [publish] targets (usually inherited from the workspace root).
func cmdPublish(args []string, ver *Version, local bool) error {
	if len(args) != 0 {
		return fmt.Errorf("pekit publish takes no arguments")
	}
	artifact, pf, wsRoot, err := buildPackage(ver, local)
	if err != nil {
		return err
	}
	if len(pf.Publish) == 0 {
		return fmt.Errorf("no [publish] targets (add [[publish.<type>]] to package.pekit.toml or the workspace root)")
	}
	// localdir paths are workspace-root-relative; without a workspace they
	// fall back to the recipe dir.
	base := wsRoot
	if base == "" {
		if base, err = os.Getwd(); err != nil {
			return err
		}
	}
	for _, t := range pf.Publish {
		switch t.Type {
		case "localdir":
			if err := publishLocalDir(artifact, filepath.Join(base, t.Path)); err != nil {
				return fmt.Errorf("publish localdir: %w", err)
			}
		default:
			return fmt.Errorf("publish: unsupported target type %q", t.Type)
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

// resolveFiles turns [files] sources into verified absolute paths:
// stage references resolve under outDir/build/<target>/, plain paths
// resolve against the project root.
func resolveFiles(pf *PackageFile, name, outDir, literalRoot string) ([]StagedFile, error) {
	files := make([]StagedFile, 0, len(pf.Files))
	for _, m := range pf.Files {
		rel := filepath.Join(literalRoot, m.Source.Path)
		if m.Source.Target != "" {
			rel = filepath.Join(outDir, "build", m.Source.Target, m.Source.Path)
		}
		abs, err := filepath.Abs(rel)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			hint := ""
			if m.Source.Target != "" {
				hint = fmt.Sprintf(" (run %q first?)", "pekit build "+m.Source.Target)
			}
			return nil, fmt.Errorf("package %s: source %q not found at %s%s",
				name, m.Source, abs, hint)
		}
		files = append(files, StagedFile{Source: abs, Dest: m.Dest})
	}
	return files, nil
}
