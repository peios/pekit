package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = "usage: pekit <build|test|install|package> [target]"

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

	stageDir := ""
	if verb == "build" && cfg.OutDir != "" {
		stageDir, err = prepareOutDir(cfg.OutDir, verb, name, cfg.ClearOut)
		if err != nil {
			return err
		}
		fmt.Printf("pekit: out: %s\n", stageDir)
		cmd.Env = append(os.Environ(), "PEKIT_OUT="+stageDir)
	}

	if err := cmd.Run(); err != nil {
		return err
	}
	if stageDir != "" && isEmptyDir(stageDir) {
		fmt.Fprintf(os.Stderr, "pekit: warning: build %s left %s empty\n", name, stageDir)
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
	name, err := targetArg(args)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig("pekit.toml")
	if err != nil {
		return err
	}

	if cfg.Packages == nil {
		return fmt.Errorf("pekit.toml has no [package] section")
	}
	pkg, ok := cfg.Packages[name]
	if !ok {
		return fmt.Errorf("no package target %q (available: %s)",
			name, strings.Join(sortedNames(cfg.Packages), ", "))
	}
	engine, err := engineFor(name, pkg)
	if err != nil {
		return err
	}

	if cfg.OutDir == "" {
		return fmt.Errorf("package %s: packaging requires outDir to be set", name)
	}
	buildStage, err := filepath.Abs(filepath.Join(cfg.OutDir, "build", name))
	if err != nil {
		return err
	}
	if !dirHasEntries(buildStage) {
		return fmt.Errorf("package %s: no build artifacts in %s (run %q first)",
			name, buildStage, "pekit build "+name)
	}
	outStage, err := prepareOutDir(cfg.OutDir, "package", name, cfg.ClearOut)
	if err != nil {
		return err
	}

	fmt.Printf("pekit: package %s (format %s)\n", name, pkg.Format)
	return engine(PackageJob{Name: name, BuildStage: buildStage, OutStage: outStage})
}
