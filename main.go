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
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "build", "test", "install":
		return cmdVerb(args[0], args[1:])
	case "package":
		return cmdPackage(args[1:])
	case "clean":
		return cmdClean(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
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

func cmdVerb(verb string, args []string) error {
	name, err := targetArg(args)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig("pekit.toml")
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
			srcDir, err := fetchSource(cfg.Source, cfg.OutDir)
			if err != nil {
				return err
			}
			cmd.Dir = srcDir
		}
		stageDir, err := prepareOutDir(cfg.OutDir, verb, name, cfg.ClearOut)
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

// sourceDir is the checkout path for a [source] block: OutDir/source/<rev>.
// The rev keys the directory — an immutable rev makes the checkout a
// valid cache; '/' in refs is flattened so the path stays one level deep.
func sourceDir(src *Source, outDir string) string {
	rev := strings.ReplaceAll(src.Rev, "/", "_")
	return filepath.Join(outDir, "source", rev)
}

// fetchSource ensures the pinned source is checked out and returns its
// absolute path. An existing checkout is reused (immutable rev → valid;
// a mutable ref may go stale — pekit clean forces a re-fetch). A failed
// checkout is torn down so the next run re-clones rather than reusing a
// half-built tree.
func fetchSource(src *Source, outDir string) (string, error) {
	abs, err := filepath.Abs(sourceDir(src, outDir))
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
func cmdClean(args []string) error {
	name, err := targetArg(args)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig("pekit.toml")
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

func cmdPackage(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("pekit package takes no arguments (one package.pekit.toml per package)")
	}

	pf, err := LoadPackageFile("package.pekit.toml")
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	name := pf.Name
	if name == "" {
		name = filepath.Base(wd)
	}

	engine, err := engineFor(pf.Format)
	if err != nil {
		return fmt.Errorf("package %s: %w", name, err)
	}

	cfg, err := LoadConfig("pekit.toml")
	if err != nil {
		return err
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
			return fmt.Errorf("package %s: [files] references build target %q but pekit.toml has no [build.%s]",
				name, targetName, targetName)
		}
		if err := runCommandTarget(cfg, "build", targetName, target); err != nil {
			return err
		}
	}

	files, err := resolveFiles(pf, name, cfg.OutDir)
	if err != nil {
		return err
	}
	outStage, err := prepareOutDir(cfg.OutDir, "package", name, cfg.ClearOut)
	if err != nil {
		return err
	}

	// Provenance identifies what built the package: the fetched source
	// for a [source] recipe (already checked out by the build above),
	// else the recipe dir.
	provenanceDir := wd
	if cfg.Source != nil {
		if provenanceDir, err = filepath.Abs(sourceDir(cfg.Source, cfg.OutDir)); err != nil {
			return err
		}
	}

	fmt.Printf("pekit: package %s (format %s, %d files)\n", name, pf.Format, len(files))
	return engine(PackageJob{Pkg: pf, Name: name, Root: wd, ProvenanceDir: provenanceDir, Files: files, OutStage: outStage})
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
func resolveFiles(pf *PackageFile, name, outDir string) ([]StagedFile, error) {
	files := make([]StagedFile, 0, len(pf.Files))
	for _, m := range pf.Files {
		rel := m.Source.Path
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
