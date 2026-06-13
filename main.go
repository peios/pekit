package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = "usage: pekit <build|test|install|clean> [target] | pekit package"

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
	args, f, err := extractFlags(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New(usage)
	}
	vers, err := resolveVersions(f.version, f.hasVersion)
	if err != nil {
		return err
	}

	// Plain single version (or none): dispatch directly so a child's exit
	// code propagates unchanged.
	if len(vers) == 1 && !f.remember {
		return dispatch(args, vers[0])
	}

	// Multiple, or --remember-built: build each, optionally skipping and
	// recording against the built-ledger, collecting failures.
	var ledger *builtLedger
	if f.remember {
		if !f.hasVersion {
			return fmt.Errorf("--remember-built needs --version (nothing to skip or record otherwise)")
		}
		if ledger, err = loadLedger("pekit.built"); err != nil {
			return err
		}
	}

	var failed []string
	built, skipped := 0, 0
	for _, ver := range vers {
		if ledger != nil && !f.bust && ledger.has(ver.Full) {
			fmt.Fprintf(os.Stderr, "pekit: %s already built, skipping (--bust to rebuild)\n", ver.Full)
			skipped++
			continue
		}
		if err := dispatch(args, ver); err != nil {
			failed = append(failed, ver.Full)
			fmt.Fprintf(os.Stderr, "pekit: %s failed: %v\n", ver.Full, err)
			continue
		}
		if ledger != nil {
			if err := ledger.add(ver.Full); err != nil {
				return fmt.Errorf("recording %s in pekit.built: %w", ver.Full, err)
			}
		}
		built++
	}
	if ledger != nil {
		fmt.Fprintf(os.Stderr, "pekit: %d built, %d skipped, %d failed\n", built, skipped, len(failed))
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d versions failed: %s", len(failed), len(vers), strings.Join(failed, ", "))
	}
	return nil
}

func dispatch(args []string, ver *Version) error {
	switch args[0] {
	case "build", "test", "install":
		return cmdVerb(args[0], args[1:], ver)
	case "package":
		return cmdPackage(args[1:], ver)
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

func cmdVerb(verb string, args []string, ver *Version) error {
	name, err := targetArg(args)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
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
func revScope(src *Source) string {
	return strings.ReplaceAll(src.Rev, "/", "_")
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

func cmdPackage(args []string, ver *Version) error {
	if len(args) != 0 {
		return fmt.Errorf("pekit package takes no arguments (one package.pekit.toml per package)")
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := LoadConfig("pekit.toml", ver)
	if err != nil {
		return err
	}

	pf, err := tryLoadPackageFile("package.pekit.toml", ver)
	if err != nil {
		return err
	}
	_, hasBuild := cfg.Commands["build"]

	// With [source], config resolves recipe-first, source-fallback: a
	// self-describing upstream (loregd) ships its own pekit files, so the
	// recipe can be just [source] and we borrow the source's build +
	// package definition. Whatever the recipe does provide shadows it.
	provenanceDir, literalRoot := wd, wd
	if cfg.Source != nil {
		checkout, aerr := filepath.Abs(sourceCheckout(cfg))
		if aerr != nil {
			return aerr
		}
		provenanceDir, literalRoot = checkout, checkout
		if pf == nil || !hasBuild {
			if _, ferr := fetchSource(cfg.Source, sourceCheckout(cfg)); ferr != nil {
				return ferr
			}
			if pf == nil {
				p, lerr := tryLoadPackageFile(filepath.Join(checkout, "package.pekit.toml"), ver)
				if lerr != nil {
					return lerr
				}
				pf = p
			}
			if !hasBuild {
				if srcCfg, cerr := LoadConfig(filepath.Join(checkout, "pekit.toml"), ver); cerr == nil {
					if b, ok := srcCfg.Commands["build"]; ok {
						cfg.Commands["build"] = b
						hasBuild = true
					}
					if len(cfg.Env) == 0 {
						cfg.Env = srcCfg.Env
					}
				}
			}
		}
	}

	if pf == nil {
		return fmt.Errorf("no package.pekit.toml found (recipe has none; [source] upstream provides none)")
	}

	name := pf.Name
	if name == "" {
		name = defaultName(cfg.Source, wd)
	}

	engine, err := engineFor(pf.Format)
	if err != nil {
		return fmt.Errorf("package %s: %w", name, err)
	}
	if cfg.OutDir == "" {
		return fmt.Errorf("package %s: packaging requires outDir in pekit.toml", name)
	}

	// Stage references name the build targets they consume, so packaging
	// can rebuild them itself and never package a stale stage. Literal
	// paths are underivable and stay the caller's freshness problem.
	for _, targetName := range referencedBuildTargets(pf) {
		target, ok := cfg.Commands["build"][targetName]
		if !ok {
			return fmt.Errorf("package %s: [files] references build target %q but no [build.%s] in recipe or source",
				name, targetName, targetName)
		}
		if err := runCommandTarget(cfg, "build", targetName, target); err != nil {
			return err
		}
	}

	files, err := resolveFiles(pf, name, outBase(cfg), literalRoot)
	if err != nil {
		return err
	}
	outStage, err := prepareOutDir(outBase(cfg), "package", name, cfg.ClearOut)
	if err != nil {
		return err
	}

	fmt.Printf("pekit: package %s (format %s, %d files)\n", name, pf.Format, len(files))
	return engine(PackageJob{Pkg: pf, Name: name, Root: wd, ProvenanceDir: provenanceDir, Files: files, OutStage: outStage})
}

// tryLoadPackageFile loads a package file, returning (nil, nil) when it
// does not exist so callers can fall back to another location.
func tryLoadPackageFile(path string, ver *Version) (*PackageFile, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	return LoadPackageFile(path, ver)
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
