package pekit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecipeTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	writeFile(t, path, `tags = ["bootstrap", "toolchain"]`)
	recipe, err := LoadRecipe(path)
	if err != nil || !reflect.DeepEqual(recipe.Tags, []string{"bootstrap", "toolchain"}) {
		t.Fatalf("tags = %v, %v", recipe.Tags, err)
	}
	for name, body := range map[string]string{
		"duplicate": `tags = ["a", "a"]`,
		"invalid":   `tags = ["not/a/tag"]`,
		"type":      `tags = "bootstrap"`,
	} {
		writeFile(t, path, body)
		if _, err := LoadRecipe(path); err == nil {
			t.Fatalf("%s tags accepted", name)
		}
	}
}

func TestTagFlagsAreWorkspaceOnly(t *testing.T) {
	inv, err := ParseInvocation([]string{"workspace", "publish", "--tag", "bootstrap", "--exclude-tag", "slow", "--env", "debian"}, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Tags, []string{"bootstrap"}) || !reflect.DeepEqual(inv.ExcludeTags, []string{"slow"}) {
		t.Fatalf("tags %v exclude %v", inv.Tags, inv.ExcludeTags)
	}
	if _, err := ParseInvocation([]string{"publish", "--tag", "bootstrap"}, "/tmp"); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("--tag outside workspace accepted: %v", err)
	}
	if _, err := ParseInvocation([]string{"workspace", "--tag", "bootstrap", "build"}, "/tmp"); err == nil {
		t.Fatal("--tag before the delegated command accepted")
	}
	if _, err := ParseInvocation([]string{"workspace", "build", "--tag", "a/b"}, "/tmp"); err == nil {
		t.Fatal("invalid tag accepted")
	}
}

func TestPackageDependencyProviderInherits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	writeFile(t, path, "format = \"peipkg\"\ndependency_provider = \"peipkg\"\n")
	base, err := LoadPackageFile(path)
	if err != nil || base.DependencyProvider != "peipkg" {
		t.Fatalf("provider = %q, %v", base.DependencyProvider, err)
	}
	merged := mergePackageConfig(base, PackageConfig{Format: "peipkg"})
	if merged.DependencyProvider != "peipkg" {
		t.Fatal("dependency_provider was not inherited")
	}
	if mergePackageConfig(base, PackageConfig{DependencyProvider: "apt"}).DependencyProvider != "apt" {
		t.Fatal("dependency_provider was not overridable")
	}
	writeFile(t, path, `dependency_provider = "a/b"`)
	if _, err := LoadPackageFile(path); err == nil {
		t.Fatal("invalid dependency_provider accepted")
	}
}

func TestBreakCyclePicksFewestUnfinishedThenID(t *testing.T) {
	if got := breakCycle(map[string]int{"c3": 2, "c2": 1, "c1": 1, "app": 3}); got != "c1" {
		t.Fatalf("breakCycle = %q", got)
	}
	if got := breakCycle(map[string]int{"b": 2, "a": 3}); got != "b" {
		t.Fatalf("breakCycle = %q", got)
	}
}

// orderingWorkspace: a-app's build needs test.lib (z-lib), whose runtime
// needs test.tool (m-tool); b-leaf needs nothing; x1 and x2 need each other.
func orderingWorkspace(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "peipkg.env.pekit.toml"), `dependency_provider = "peipkg"`)
	writeFile(t, filepath.Join(dir, "apt.env.pekit.toml"), `dependency_provider = "apt"`)
	member := func(name, needs, pkg, runtime, provider string, delay string) {
		deps := ""
		if needs != "" {
			deps = fmt.Sprintf("\n[build.main.dependencies.peipkg]\n%q = \"*\"\n", needs)
		}
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), fmt.Sprintf(`
out_dir = "out"

[build.main]
command = 'printf "start %s\n" >> %q; sleep %s; printf "end %s\n" >> %q'
%s`, name, log, delay, name, log, deps))
		if pkg == "" {
			return
		}
		body := fmt.Sprintf("format = \"tar\"\n[package]\nname = %q\n", pkg)
		if provider != "" {
			body = fmt.Sprintf("dependency_provider = %q\n", provider) + body
		}
		if runtime != "" {
			body += fmt.Sprintf("[dependencies]\n%q = \"*\"\n", runtime)
		}
		writeFile(t, filepath.Join(dir, name, "package.pekit.toml"), body)
	}
	member("a-app", "test.lib", "", "", "", "0")
	member("b-leaf", "", "", "", "", "0")
	member("m-tool", "", "test.tool", "", "peipkg", "0.3")
	member("z-lib", "", "test.lib", "test.tool", "peipkg", "0.3")
	member("x1", "test.x2", "test.x1", "", "peipkg", "0")
	member("x2", "test.x1", "test.x2", "", "peipkg", "0")
	return dir, log
}

func runWorkspaceIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err := app.Run(args)
	return stdout.String() + stderr.String(), err
}

func TestWorkspaceOrdersByGateAndRuntimeDependencies(t *testing.T) {
	dir, log := orderingWorkspace(t)
	out, err := runWorkspaceIn(t, dir, "--dry-run", "workspace", "build", "--env", "peipkg")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"after m-tool, z-lib", "x2", "after x1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run order lacks %q:\n%s", want, out)
		}
	}
	// A provider whose names no member defines orders nothing.
	out, err = runWorkspaceIn(t, dir, "--dry-run", "workspace", "build", "--env", "apt")
	if err != nil || strings.Contains(out, "after ") {
		t.Fatalf("apt provider produced an order (%v):\n%s", err, out)
	}

	out, err = runWorkspaceIn(t, dir, "workspace", "--jobs", "4", "build", "--env", "peipkg")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	at := func(line string) int {
		for i, l := range lines {
			if l == line {
				return i
			}
		}
		t.Fatalf("log lacks %q:\n%s", line, data)
		return -1
	}
	if at("start a-app") < at("end z-lib") || at("start a-app") < at("end m-tool") {
		t.Fatalf("a-app started before its producers finished:\n%s", data)
	}
	if at("start x2") < at("end x1") {
		t.Fatalf("cycle members were not run in id order:\n%s", data)
	}
}

func TestWorkspaceTagSelection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	for name, tags := range map[string]string{"seed": `["bootstrap"]`, "world": `[]`, "tool": `["bootstrap", "slow"]`} {
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), fmt.Sprintf(`
tags = %s
out_dir = "out"

[build]
command = 'printf built > "$PEKIT_OUT/result"'
`, tags))
	}
	built := func() []string {
		var out []string
		for _, name := range []string{"seed", "tool", "world"} {
			if fileExists(filepath.Join(dir, name, "out", "build", "main", "result")) {
				out = append(out, name)
			}
			_ = os.RemoveAll(filepath.Join(dir, name, "out"))
		}
		return out
	}
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"--tag", "bootstrap"}, []string{"seed", "tool"}},
		{[]string{"--exclude-tag", "bootstrap"}, []string{"world"}},
		{[]string{"--tag", "bootstrap", "--exclude-tag", "slow"}, []string{"seed"}},
	} {
		out, err := runWorkspaceIn(t, dir, append([]string{"workspace", "build"}, c.args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", c.args, err, out)
		}
		if got := built(); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v built %v, want %v", c.args, got, c.want)
		}
	}
	if out, err := runWorkspaceIn(t, dir, "workspace", "build", "--tag", "missing"); err == nil {
		t.Fatalf("unmatched tag accepted:\n%s", out)
	}
}
