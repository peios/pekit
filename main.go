package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const usage = "usage: pekit <build|install> [target]"

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
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func cmdVerb(verb string, args []string) error {
	if len(args) > 1 {
		return errors.New(usage)
	}

	cfg, err := LoadConfig("pekit.toml")
	if err != nil {
		return err
	}

	targets, ok := cfg.Sections[verb]
	if !ok {
		return fmt.Errorf("pekit.toml has no [%s] section", verb)
	}

	name := "main"
	if len(args) == 1 {
		name = args[0]
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
	return cmd.Run()
}
