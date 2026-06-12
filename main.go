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
	engine, err := engineFor(pf.Format)
	if err != nil {
		return fmt.Errorf("package %s: %w", pf.Name, err)
	}

	cfg, err := LoadConfig("pekit.toml")
	if err != nil {
		return err
	}
	if cfg.OutDir == "" {
		return fmt.Errorf("package %s: packaging requires outDir in pekit.toml", pf.Name)
	}

	files, err := resolveFiles(pf, cfg.OutDir)
	if err != nil {
		return err
	}
	outStage, err := prepareOutDir(cfg.OutDir, "package", pf.Name, cfg.ClearOut)
	if err != nil {
		return err
	}

	fmt.Printf("pekit: package %s (format %s, %d files)\n", pf.Name, pf.Format, len(files))
	return engine(PackageJob{Pkg: pf, Files: files, OutStage: outStage})
}

// resolveFiles turns [files] sources into verified absolute paths:
// stage references resolve under outDir/build/<target>/, plain paths
// resolve against the project root.
func resolveFiles(pf *PackageFile, outDir string) ([]StagedFile, error) {
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
				pf.Name, m.Source, abs, hint)
		}
		files = append(files, StagedFile{Source: abs, Dest: m.Dest})
	}
	return files, nil
}
