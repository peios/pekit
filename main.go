package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const usage = "usage: pekit <build|install|package> [target]"

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
	case "build", "install":
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

	fmt.Printf("pekit: %s %s: %s\n", verb, name, target.Command)
	cmd := exec.Command("sh", "-c", target.Command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	stageDir := ""
	if verb == "build" && cfg.OutDir != "" {
		stageDir, err = prepareOutDir(cfg.OutDir, name, cfg.ClearOut)
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

	fmt.Printf("pekit: package %s (format %s)\n", name, pkg.Format)
	return buildPackage(name, pkg)
}
