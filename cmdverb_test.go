package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerbTargets(t *testing.T) {
	bare := map[string]Target{"main": {Command: "x"}}
	named := map[string]Target{"cli": {Command: "x"}, "server": {Command: "x"}}

	join := func(args []string, targets map[string]Target) string {
		got, err := verbTargets("install", args, targets)
		if err != nil {
			t.Fatalf("verbTargets(%v): %v", args, err)
		}
		return strings.Join(got, ",")
	}
	if got := join(nil, bare); got != "main" {
		t.Errorf("bare, no arg = %q, want main", got)
	}
	if got := join(nil, named); got != "cli,server" {
		t.Errorf("named, no arg = %q, want all (cli,server)", got)
	}
	if got := join([]string{"server"}, named); got != "server" {
		t.Errorf("named, one arg = %q, want server", got)
	}
	if _, err := verbTargets("install", []string{"nope"}, named); err == nil {
		t.Error("an unknown target should error")
	}
	if _, err := verbTargets("install", []string{"a", "b"}, named); err == nil {
		t.Error("two targets should be a usage error")
	}
}

// installRecipe writes a recipe whose named install targets each touch a
// sentinel file, so a test can see which ran.
func installRecipe(t *testing.T, dir string, sentinels map[string]string) {
	t.Helper()
	var b strings.Builder
	for _, name := range sortedKeysStr(sentinels) {
		fmt.Fprintf(&b, "[install.%s]\ncommand = \"echo done > '%s'\"\n\n", name, sentinels[name])
	}
	writeFile(t, filepath.Join(dir, "pekit.toml"), b.String())
}

func sortedKeysStr(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	// small, fixed sets in tests; keep deterministic
	for i := 0; i < len(ks); i++ {
		for j := i + 1; j < len(ks); j++ {
			if ks[j] < ks[i] {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return ks
}

// TestBareInstallFansOut: with only named install targets, a bare
// `pekit install` runs every one.
func TestBareInstallFansOut(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "cli.done")
	srv := filepath.Join(dir, "server.done")
	installRecipe(t, dir, map[string]string{"cli": cli, "server": srv})
	t.Chdir(dir)

	if err := run([]string{"install"}); err != nil {
		t.Fatalf("bare install: %v", err)
	}
	for _, s := range []string{cli, srv} {
		if _, err := os.Stat(s); err != nil {
			t.Errorf("expected %s to have run: %v", filepath.Base(s), err)
		}
	}
}

// TestNamedInstallRunsOne: naming a target runs only that one.
func TestNamedInstallRunsOne(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "cli.done")
	srv := filepath.Join(dir, "server.done")
	installRecipe(t, dir, map[string]string{"cli": cli, "server": srv})
	t.Chdir(dir)

	if err := run([]string{"install", "cli"}); err != nil {
		t.Fatalf("install cli: %v", err)
	}
	if _, err := os.Stat(cli); err != nil {
		t.Errorf("cli should have run: %v", err)
	}
	if _, err := os.Stat(srv); err == nil {
		t.Error("server should not run when cli is named")
	}
}

// TestRunRemoteNamedTarget: `pekit install <remote> cli` clones the recipe and
// runs only its [install.cli] target — the named target survives spec removal.
func TestRunRemoteNamedTarget(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	out := t.TempDir()
	cli := filepath.Join(out, "cli.done")
	srv := filepath.Join(out, "server.done")
	recipe := fmt.Sprintf("[install.cli]\ncommand = \"echo done > '%s'\"\n\n[install.server]\ncommand = \"echo done > '%s'\"\n", cli, srv)
	initRepo(t, origin, map[string]string{"pekit.toml": recipe})

	spec := "file://" + origin
	t.Cleanup(func() { os.RemoveAll(remoteCacheDir(spec)) })

	if err := run([]string{"install", spec, "cli"}); err != nil {
		t.Fatalf("install %s cli: %v", spec, err)
	}
	if _, err := os.Stat(cli); err != nil {
		t.Errorf("remote install cli should have run: %v", err)
	}
	if _, err := os.Stat(srv); err == nil {
		t.Error("remote install cli should not run the server target")
	}
}
