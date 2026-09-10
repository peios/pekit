package pekit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestBuildAndPackageTar(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.main]
command = "mkdir -p \"$PEKIT_OUT/bin\" && printf hello > \"$PEKIT_OUT/bin/hello\""
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
":bin/hello" = "usr/bin/hello"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected one artifact, got %v", artifacts)
	}
	assertTarHas(t, artifacts[0], "usr/bin/hello")
}

// A directory source that stages an empty directory must pack as an
// explicit empty-directory payload entry rather than vanishing. This is
// the fsbase skeleton case (runtime mountpoint dirs, no files); without it
// the package resolves to zero payload entries and fails "empty_package".
func TestBuildAndPackageEmptyDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.main]
command = "for d in dev proc var; do mkdir -p \"$PEKIT_OUT/$d\"; done"
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
":dev"  = "dev"
":proc" = "proc"
":var"  = "var"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected one artifact, got %v", artifacts)
	}
	for _, want := range []string{"dev/", "proc/", "var/"} {
		assertTarHas(t, artifacts[0], want)
	}
}

func TestMemberPackageDoesNotInheritBaseEmittedName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[package]
name = "base-name"
`)
	writeFile(t, filepath.Join(dir, "child.package.pekit.toml"), `
[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "child"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	child, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "child.tar"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "base-name.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(child) != 1 || len(base) != 0 {
		t.Fatalf("unexpected artifacts child=%v base=%v", child, base)
	}
}

func TestDryRunJSONEmitsSinglePlanObject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "false"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--dry-run", "--json"}); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("expected one JSON document, got %d: %s", len(lines), stdout.String())
	}
	var doc struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Events  []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(lines[0], &doc); err != nil {
		t.Fatalf("invalid JSON plan: %v\n%s", err, stdout.String())
	}
	if doc.Type != "plan" || doc.Command != "build" || len(doc.Events) == 0 {
		t.Fatalf("unexpected plan doc: %#v", doc)
	}
}

func TestDryRunJSONReportsSuppressedUnusedFlag(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"clean", "--version", "1.2.3", "--allow-unused", "--dry-run", "--json"}); err != nil {
		t.Fatalf("dry-run failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"type":"unused_suppressed"`)) {
		t.Fatalf("expected suppressed unused flag event, got %s", stdout.String())
	}
}

func TestMainJSONDryRunErrorDoesNotPrintPlainStderr(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "https://example.invalid/repo.git"
ref = "v{{version}}"

[build]
command = "true"
`)
	var stdout, stderr bytes.Buffer
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	code := Main([]string{"build", "--dry-run", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected JSON dry-run error to stay off stderr, got %q", stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"type":"plan"`)) || !bytes.Contains(stdout.Bytes(), []byte(`"type":"error"`)) {
		t.Fatalf("expected plan with error event, got %s", stdout.String())
	}
}

func TestMainJSONRuntimeErrorDoesNotPrintPlainStderr(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "exit 9"
`)
	var stdout, stderr bytes.Buffer
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	code := Main([]string{"build", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected JSON runtime error to stay off stderr, got %q", stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"type":"error"`)) {
		t.Fatalf("expected JSON error event, got %s", stdout.String())
	}
}

func TestQuietKeepsTargetOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf visible"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--quiet"}); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("visible")) {
		t.Fatalf("quiet suppressed target output: stdout=%q", stdout.String())
	}
}

func TestQuietReportsArtifactPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "--quiet"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("main.tar")) {
		t.Fatalf("quiet package output did not include artifact path: %q", stdout.String())
	}
}

func TestArrayWrapperUsesCommandPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf wrapped > \"$PEKIT_OUT/result\""
`)
	writeFile(t, filepath.Join(dir, "env.pekit.toml"), `
[wrap]
command = ["sh", "-euc", "{{command}}"]
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "wrapped" {
		t.Fatalf("wrapper did not run inner command: %q", string(data))
	}
}

func TestBuildDependenciesAreExportedForSelectedProvider(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = 'cp "$PEKIT_DEPENDENCIES_FILE" "$PEKIT_OUT/deps.json" && printf "%s" "$PEKIT_DEPENDENCIES" > "$PEKIT_OUT/deps.txt" && printf "%s" "$PEKIT_DEPENDENCY_PROVIDER" > "$PEKIT_OUT/provider"'

[build.dependencies.peipkg]
tool-devel = "{{version}}"

[build.dependencies.apt]
"g++" = ">= 15"
libtool-dev = "*"
`)
	writeFile(t, filepath.Join(dir, "env.pekit.toml"), `dependency_provider = "apt"`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--version", "1.2.3"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "deps.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload DependencyPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("dependency payload is not JSON: %v\n%s", err, string(data))
	}
	if payload.Provider != "apt" || payload.Dependencies["g++"] != ">= 15" || payload.Dependencies["libtool-dev"] != "*" {
		t.Fatalf("unexpected selected dependencies: %#v", payload)
	}
	if payload.AllProviders["peipkg"]["tool-devel"] != "1.2.3" {
		t.Fatalf("peipkg dependencies were not rendered: %#v", payload.AllProviders)
	}
	list, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "deps.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(list, []byte("g++ >= 15\n")) || !bytes.Contains(list, []byte("libtool-dev *\n")) {
		t.Fatalf("unexpected dependency list: %q", string(list))
	}
	provider, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "provider"))
	if err != nil {
		t.Fatal(err)
	}
	if string(provider) != "apt" {
		t.Fatalf("provider export = %q", string(provider))
	}
}

func TestWorkspaceNamedEnvFileIsInheritedByMember(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "ci.env.pekit.toml"), `
dependency_provider = "workspace"

[env]
FROM_WORKSPACE_PROFILE = "yes"

[wrap]
command = ["env", "WRAPPED_BY=workspace-profile", "sh", "-euc", "{{command}}"]
`)
	member := filepath.Join(dir, "member")
	writeFile(t, filepath.Join(member, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf "%s|%s|%s" "$FROM_WORKSPACE_PROFILE" "$WRAPPED_BY" "$PEKIT_DEPENDENCY_PROVIDER" > "$PEKIT_OUT/result"'

[build.dependencies.workspace]
tool = "*"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(member); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--env", "ci"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(member, "out", "build", "main", "result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "yes|workspace-profile|workspace" {
		t.Fatalf("workspace profile was not inherited: %q", string(data))
	}
}

func TestRecipeNamedEnvFileOverridesWorkspaceEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "ci.env.pekit.toml"), `
dependency_provider = "workspace"

[env]
WORKSPACE_ONLY = "preserved"
SHARED = "workspace"

[wrap]
command = ["env", "WRAPPED_BY=workspace", "sh", "-euc", "{{command}}"]
`)
	member := filepath.Join(dir, "member")
	writeFile(t, filepath.Join(member, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf "%s|%s|%s|%s" "$WORKSPACE_ONLY" "$SHARED" "$WRAPPED_BY" "$PEKIT_DEPENDENCY_PROVIDER" > "$PEKIT_OUT/result"'

[build.dependencies.workspace]
workspace-tool = "*"

[build.dependencies.recipe]
recipe-tool = "*"
`)
	writeFile(t, filepath.Join(member, "ci.env.pekit.toml"), `
dependency_provider = "recipe"

[env]
SHARED = "recipe"

[wrap]
command = ["env", "WRAPPED_BY=recipe", "sh", "-euc", "{{command}}"]
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(member); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--env", "ci"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(member, "out", "build", "main", "result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "preserved|recipe|recipe|recipe" {
		t.Fatalf("recipe profile did not override workspace profile: %q", string(data))
	}
}

func TestWorkspaceEnvFileSymlinkIsNotAppliedTwice(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `
include = ["./*"]

[env]
CHAIN = "base"
`)
	workspaceProfile := filepath.Join(dir, "ci.env.pekit.toml")
	writeFile(t, workspaceProfile, `
[env]
CHAIN = "$CHAIN/workspace"
`)
	member := filepath.Join(dir, "member")
	writeFile(t, filepath.Join(member, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf "%s" "$CHAIN" > "$PEKIT_OUT/result"'
`)
	if err := os.Symlink(filepath.Join("..", "ci.env.pekit.toml"), filepath.Join(member, "ci.env.pekit.toml")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(member); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--env", "ci"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(member, "out", "build", "main", "result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base/workspace" {
		t.Fatalf("shared profile applied more than once: %q", string(data))
	}
}

func TestNamedEnvMustExistInWorkspaceOrRecipe(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	member := filepath.Join(dir, "member")
	writeFile(t, filepath.Join(member, "pekit.toml"), `
out_dir = "out"

[build]
command = "true"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(member); err != nil {
		t.Fatal(err)
	}
	err := app.Run([]string{"build", "--env", "ci"})
	if diagCode(err) != "missing_env_file" {
		t.Fatalf("err = %v, want missing_env_file", err)
	}
}

func TestBuildDependenciesRequireSelectedProvider(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf ran > "$PEKIT_OUT/marker"'

[build.dependencies.peipkg]
gmp-devel = ">= 6.3.0"
`)
	writeFile(t, filepath.Join(dir, "env.pekit.toml"), `dependency_provider = "apt"`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err := app.Run([]string{"build"})
	if err == nil {
		t.Fatal("expected missing selected dependency provider to fail")
	}
	if diagCode(err) != "missing_dependency_provider" {
		t.Fatalf("err = %v, want missing_dependency_provider", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "out", "build", "main", "marker")); !os.IsNotExist(statErr) {
		t.Fatalf("target command should not have run; stat err = %v", statErr)
	}
}

func TestTargetTimestampExportsAreUnixSeconds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf "$PEKIT_BUILD_TIMESTAMP:$PEKIT_SOURCE_TIMESTAMP" > "$PEKIT_OUT/timestamps"'
`)
	app := &App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Now:    func() time.Time { return time.Unix(1234, 0).UTC() },
	}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "timestamps"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1234:0" {
		t.Fatalf("unexpected timestamps %q", string(data))
	}
}

func TestDelegatedSourceEnvAndWrap(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(source, "pekit.toml"), `
[env]
FROM_SOURCE = "pekit"
`)
	writeFile(t, filepath.Join(source, "env.pekit.toml"), `
[wrap]
command = ["env", "WRAPPED=yes", "sh", "-euc", "{{command}}"]

[env]
FROM_SOURCE = "envfile"
`)
	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[delegate]
env = true
wrap = true

[source.local]
path = "../src"

[build]
command = 'printf "$FROM_SOURCE:$WRAPPED" > "$PEKIT_OUT/result"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--local"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "local-*", "build", "main", "result"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected delegated env output, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "envfile:yes" {
		t.Fatalf("unexpected delegated env/wrap output %q", string(data))
	}
}

func TestUserEnvValuesExpandManagedExports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[env]
TOOL_PATH = "$PEKIT_ROOT/tools"

[build]
command = 'printf "$TOOL_PATH" > "$PEKIT_OUT/tool_path"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "tool_path"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != filepath.Join(dir, "out", "tools") {
		t.Fatalf("env value did not expand managed export: %q", string(data))
	}
}

// Multi-word env values used to be emitted bare into the export prelude, so the
// shell word-split them and `export CFLAGS=-O2 -pipe` failed outright. They are
// the ordinary case for a distro flag set, and must coexist with the expansion
// TestUserEnvValuesExpandManagedExports covers.
func TestUserEnvValuesSurviveWordSplitting(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[env]
CFLAGS = "-O2 -pipe -Wp,-D_FORTIFY_SOURCE=3"
CXXFLAGS = "$CFLAGS -Wp,-D_GLIBCXX_ASSERTIONS"

[build]
command = 'printf "%s" "$CXXFLAGS" > "$PEKIT_OUT/cxxflags"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "cxxflags"))
	if err != nil {
		t.Fatal(err)
	}
	want := "-O2 -pipe -Wp,-D_FORTIFY_SOURCE=3 -Wp,-D_GLIBCXX_ASSERTIONS"
	if string(data) != want {
		t.Fatalf("multi-word env value mangled:\n got %q\nwant %q", string(data), want)
	}
}

func TestUserEnvValuesCanReferenceEarlierEnvValues(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[env]
Z_BASE = "base"
A_CHILD = "$Z_BASE/child"

[build]
command = 'printf "$A_CHILD" > "$PEKIT_OUT/value"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "main", "value"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "base/child" {
		t.Fatalf("env value did not expand earlier env value: %q", string(data))
	}
}

func TestVerboseEmitsPlanningDetails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf payload > \"$PEKIT_OUT/payload\""
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
":payload" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "--verbose"}); err != nil {
		t.Fatalf("package failed: %v", err)
	}
	output := stdout.String()
	for _, want := range []string{"out_dir=out", "selected packages", " -> usr/share/payload"} {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Fatalf("verbose output missing %s: %s", want, output)
		}
	}
}

func TestNoBuildPrunesDependencySubtree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.dep]
command = "printf dep > \"$PEKIT_OUT/ran\""

[build.main]
needs = ["dep"]
command = "printf main > \"$PEKIT_OUT/ran\""
`)
	if err := os.MkdirAll(filepath.Join(dir, "out", "build", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "out", "build", "main", "prebuilt"), "yes")
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "main", "--no-build"}); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if fileExists(filepath.Join(dir, "out", "build", "dep", "ran")) {
		t.Fatal("dependency subtree was built despite reused selected target")
	}
}

func TestNoBuildRefusesMissingStage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.main]
command = "printf ran > \"$PEKIT_OUT/ran\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err := app.Run([]string{"build", "main", "--no-build"})
	if diagCode(err) != "missing_stage" {
		t.Fatalf("want missing_stage, got %v", err)
	}
	if fileExists(filepath.Join(dir, "out", "build", "main", "ran")) {
		t.Fatal("target ran despite --no-build and missing stage")
	}
}

func TestDirectBuildDependencyOutputExport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.main]
command = "printf main > \"$PEKIT_OUT/value\""

[build.child]
needs = ["main"]
command = "cat \"$PEKIT_MAIN_OUT/value\" > \"$PEKIT_OUT/copied\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "child"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "build", "child", "copied"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "main" {
		t.Fatalf("unexpected copied dependency output %q", string(data))
	}
}

func TestBareTestRequiresMainTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[test.unit]
command = "true"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"test"}); err == nil {
		t.Fatal("expected bare test without test.main to fail")
	}
	if err := app.Run([]string{"test", "unit"}); err != nil {
		t.Fatalf("explicit test target failed: %v", err)
	}
}

func TestCleanRunsTargetThenRemovesOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[clean]
command = "printf cleaned > cleaned.txt"
`)
	if err := os.MkdirAll(filepath.Join(dir, "out", "build", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"clean"}); err != nil {
		t.Fatalf("clean failed: %v", err)
	}
	if !fileExists(filepath.Join(dir, "cleaned.txt")) {
		t.Fatal("clean target did not run")
	}
	if dirExists(filepath.Join(dir, "out")) {
		t.Fatal("managed output was not removed")
	}
}

func TestCleanRemovesOutDirSymlinkWithoutFollowing(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "recipe")
	outside := filepath.Join(base, "shared-output")
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(outside, "keep"), "preserved")
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"clean"}); err != nil {
		t.Fatalf("clean failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("managed output symlink remains: %v", err)
	}
	if !fileExists(filepath.Join(outside, "keep")) {
		t.Fatal("clean followed the output symlink")
	}
}

func TestCleanRejectsSymlinkedOutDirParent(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "recipe")
	outside := filepath.Join(base, "shared-output")
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "linked/out"`)
	writeFile(t, filepath.Join(outside, "out", "keep"), "preserved")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err := app.Run([]string{"clean"})
	if err == nil || diagCode(err) != "invalid_path" {
		t.Fatalf("expected invalid_path, got %v", err)
	}
	if !fileExists(filepath.Join(outside, "out", "keep")) {
		t.Fatal("clean followed a symlinked output parent")
	}
}

func TestSourcelessRecipeAllowsUnusedLocalFlag(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf ok > \"$PEKIT_OUT/result\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--local", "--allow-unused"}); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if !fileExists(filepath.Join(dir, "out", "build", "main", "result")) {
		t.Fatal("build output missing")
	}
}

func TestMultipackInstanceSelection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.locale]
command = "mkdir -p \"$PEKIT_OUT/locale/de_DE\" \"$PEKIT_OUT/locale/fr_FR\" && printf de > \"$PEKIT_OUT/locale/de_DE/LC\" && printf fr > \"$PEKIT_OUT/locale/fr_FR/LC\""
`)
	writeFile(t, filepath.Join(dir, "packages.pekit/lang.package.pekit.toml"), `
format = "tar"

[multipack.enum.files]
path = "locale:locale/*"
regex = '^([a-z]+)_'

[files]
"locale:locale/{{multipack}}_*/**" = "usr/lib/locale"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "lang:fr"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	fr, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "lang-fr.tar"))
	if err != nil {
		t.Fatal(err)
	}
	de, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "lang-de.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fr) != 1 || len(de) != 0 {
		t.Fatalf("unexpected multipack artifacts fr=%v de=%v", fr, de)
	}
}

func TestMultipackLiteralEnum(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "mono.txt"), "mono")
	writeFile(t, filepath.Join(dir, "serif.txt"), "serif")
	writeFile(t, filepath.Join(dir, "packages.pekit/font.package.pekit.toml"), `
format = "tar"

[multipack]
enum = ["mono", "serif"]

[files]
"@recipe:{{multipack}}.txt" = "usr/share/fonts/{{multipack}}.txt"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "font:mono"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	mono, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "font-mono.tar"))
	if err != nil {
		t.Fatal(err)
	}
	serif, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "font-serif.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mono) != 1 || len(serif) != 0 {
		t.Fatalf("unexpected literal multipack artifacts mono=%v serif=%v", mono, serif)
	}
}

func TestPackageTemplateUnavailableVariableFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[package]
name = "pkg-{{multipack}}"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err == nil {
		t.Fatal("expected unavailable multipack template to fail")
	}
}

func TestMultipackRepeatedExplicitNameFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "mono.txt"), "mono")
	writeFile(t, filepath.Join(dir, "serif.txt"), "serif")
	writeFile(t, filepath.Join(dir, "packages.pekit/font.package.pekit.toml"), `
format = "tar"

[multipack]
enum = ["mono", "serif"]

[package]
name = "font"

[files]
"@recipe:{{multipack}}.txt" = "usr/share/fonts/{{multipack}}.txt"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "font"}); err == nil {
		t.Fatal("expected repeated explicit multipack name to fail")
	}
}

func TestDryRunMultipackFromBuildOutputCanBeUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.locale]
command = "mkdir -p \"$PEKIT_OUT/locale/de_DE\""
`)
	writeFile(t, filepath.Join(dir, "packages.pekit/lang.package.pekit.toml"), `
format = "tar"

[multipack.enum.files]
path = "locale:locale/*"
regex = '^([a-z]+)_'

[files]
"locale:locale/{{multipack}}_*/**" = "usr/lib/locale"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "lang", "--dry-run"}); err != nil {
		t.Fatalf("dry-run should allow unresolved multipack: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
}

func TestDryRunDelegatedPackageDiscoveryCanBeUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"
delegate = true

[source.git]
url = "https://example.invalid/repo.git"
ref = "main"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "--dry-run"}); err != nil {
		t.Fatalf("dry-run should allow unresolved delegated packages: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
}

func TestPeipkgFileOverrideBypassesLayoutValidation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"
`)
	writeFile(t, filepath.Join(dir, "barelib.so"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
version = "1.0.0-1"
architecture = "x86_64"
description = "override package"
license = "MIT"

[files]
"@recipe:barelib.so" = { path = "lib/barelib.so", override = true }
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main_1.0.0-1_x86_64.peipkg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected peipkg artifact, got %v", artifacts)
	}
}

func TestPeipkgMetadataTemplatesPreserveArbitraryNumericCore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"
`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
version = "{{version}}-1"
architecture = "x86_64"
description = "runtime {{version}}"
license = "MIT"

[dependencies]
runtime = "{{version}}"

[provides]
"runtime-{{version}}" = "{{version}}"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package", "--version", "0.5.13.10"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main_0.5.13.10-1_x86_64.peipkg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected peipkg artifact, got %v", artifacts)
	}
	manifest := readPeipkgManifest(t, artifacts[0])
	if manifest.Description != "runtime 0.5.13.10" {
		t.Fatalf("description = %q", manifest.Description)
	}
	if len(manifest.Dependencies) != 1 || manifest.Dependencies[0].Name != "runtime" || manifest.Dependencies[0].Constraint != "0.5.13.10" {
		t.Fatalf("dependencies = %#v", manifest.Dependencies)
	}
	if len(manifest.Provides) != 1 || manifest.Provides[0].Name != "runtime-0.5.13.10" || manifest.Provides[0].Version != "0.5.13.10" {
		t.Fatalf("provides = %#v", manifest.Provides)
	}
}

func TestPeipkgRequiredFieldsFailBeforeBuild(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf built > built.txt"
`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"
builds = ["main"]

[package]
version = "1.0.0-1"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err == nil {
		t.Fatal("expected missing peipkg architecture to fail")
	}
	if fileExists(filepath.Join(dir, "built.txt")) {
		t.Fatal("build ran before peipkg metadata planning error")
	}
}

func TestPeipkgRequiresLicense(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf built > built.txt"
`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"
builds = ["main"]

[package]
version = "1.0.0-1"
architecture = "x86_64"
description = "unlicensed"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	err := app.Run([]string{"package"})
	if err == nil {
		t.Fatal("expected missing peipkg license to fail")
	}
	if diagCode(err) != "missing_package_field" || !strings.Contains(err.Error(), "license") {
		t.Fatalf("expected missing license error, got %v", err)
	}
	if fileExists(filepath.Join(dir, "built.txt")) {
		t.Fatal("build ran before peipkg metadata planning error")
	}
}

func TestGitLatestVersionSelection(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v0.5.13.9")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload2")
	runTestCmd(t, repo, "git", "commit", "-am", "second")
	runTestCmd(t, repo, "git", "tag", "v0.5.13.10")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"
tag_regex = '^v[0-9]+(?:\.[0-9]+)*$'

[build]
command = 'printf "$PEKIT_VERSION|$PEKIT_VERSION_MAJOR|$PEKIT_VERSION_MINOR|$PEKIT_VERSION_PATCH" > "$PEKIT_OUT/version.txt"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--latest"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "git-*", "build", "main", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one version output, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0.5.13.10|0|5|13" {
		t.Fatalf("latest version = %q", string(data))
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("0.5.13.10")
	if entry == nil || entry.Ref != "v0.5.13.10" || entry.Commit == "" {
		t.Fatalf("arbitrary-core version was not locked to its tag: %#v", entry)
	}
}

func TestGitLatestVersionSelectionFromNamedTagComponents(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "20260810")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload2")
	runTestCmd(t, repo, "git", "commit", "-am", "second")
	runTestCmd(t, repo, "git", "tag", "20260905")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "{{major}}{{minor}}{{patch}}"
tag_regex = '^(?P<major>[0-9]{4})(?P<minor>[0-9]{2})(?P<patch>[0-9]{2})$'

[build]
command = 'printf "$PEKIT_VERSION" > "$PEKIT_OUT/version.txt"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--latest"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "git-*", "build", "main", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one version output, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "2026.09.05" {
		t.Fatalf("latest version = %q, want 2026.09.05", string(data))
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("2026.09.05")
	if entry == nil || entry.Ref != "20260905" || entry.Commit == "" {
		t.Fatalf("dotted version was not locked to its compact tag: %#v", entry)
	}
}

func TestVersionTemplateRequiresSelectedVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "https://example.invalid/repo.git"
ref = "v{{version}}"

[build]
command = "true"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--dry-run"}); err == nil {
		t.Fatal("expected version template without version to fail")
	}
}

func TestUnsupportedVersionConstraintErrors(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v1.0.0")
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = "true"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--version", "^1.0"}); err == nil {
		t.Fatal("expected unsupported constraint to fail")
	}
}

func TestGitExactVersionTrailingZeroLadderAndManifest(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v2.43")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = 'printf "$PEKIT_VERSION" > "$PEKIT_OUT/version.txt"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--version", "2.43.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "git-*", "build", "main", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one version output, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "2.43" {
		t.Fatalf("ladder version = %q", string(data))
	}
	manifests, err := filepath.Glob(filepath.Join(recipe, "out", "git-*", "source.pekit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected source manifest, got %v", manifests)
	}
}

func TestPreferLocalAllowUnusedWithLatestUsesRemote(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	local := filepath.Join(dir, "local")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v1.0.0")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[source.local]
path = "../local"

[build]
command = 'printf "$PEKIT_SOURCE_ROOT" > "$PEKIT_OUT/source.txt"'
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--prefer-local", "--latest", "--allow-unused"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "git-*", "build", "main", "source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected remote source output, got %v", matches)
	}
}

func TestPackageDestinationCollisionFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:a.txt" = "usr/share/same"
"@recipe:b.txt" = "usr/share/same"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err == nil {
		t.Fatal("expected destination collision to fail")
	}
}

func TestPackageSourceRefCannotEscapeRoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:../outside.txt" = "usr/share/outside"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err == nil {
		t.Fatal("expected escaping package source ref to fail")
	}
}

func TestMissingStaticPackageFileFailsBeforeBuild(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf built > built.txt"
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
builds = ["main"]

[files]
"@recipe:missing.txt" = "usr/share/missing"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err == nil {
		t.Fatal("expected missing static package file to fail")
	}
	if fileExists(filepath.Join(dir, "built.txt")) {
		t.Fatal("build ran before static package file planning error")
	}
}

func TestPackageExcludesMatchSourcePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "share", "keep.txt"), "keep")
	writeFile(t, filepath.Join(dir, "share", "drop.tmp"), "drop")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
excludes = ["@recipe:share/*.tmp"]

[files]
"@recipe:share/**" = "usr/share/app/"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected package artifact, got %v", artifacts)
	}
	assertTarHas(t, artifacts[0], "usr/share/app/keep.txt")
	assertTarNotHas(t, artifacts[0], "usr/share/app/drop.tmp")
}

func TestGlobbedDirectoryMatchesPreserveRelativeDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "locale", "de_AT", "LC_ADDRESS"), "de_AT")
	writeFile(t, filepath.Join(dir, "locale", "de_AT.utf8", "LC_ADDRESS"), "de_AT.utf8")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:locale/de_*/**" = "usr/lib/locale/"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected package artifact, got %v", artifacts)
	}
	assertTarHas(t, artifacts[0], "usr/lib/locale/de_AT/LC_ADDRESS")
	assertTarHas(t, artifacts[0], "usr/lib/locale/de_AT.utf8/LC_ADDRESS")
}

func TestGlobDestinationWithoutTrailingSlashIsDirectoryPrefix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "share", "man8", "a.8"), "a")
	writeFile(t, filepath.Join(dir, "share", "man8", "b.8"), "b")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:share/man8/*" = "usr/share/man/man8"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected package artifact, got %v", artifacts)
	}
	assertTarHas(t, artifacts[0], "usr/share/man/man8/a.8")
	assertTarHas(t, artifacts[0], "usr/share/man/man8/b.8")
}

func TestGlobbedSymlinkDirectoryIsNotTraversed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "real", "payload.txt"), "payload")
	if err := os.MkdirAll(filepath.Join(dir, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real", filepath.Join(dir, "links", "tree")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:links/**" = "usr/share/app"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected package artifact, got %v", artifacts)
	}
	assertTarHas(t, artifacts[0], "usr/share/app/tree")
	assertTarNotHas(t, artifacts[0], "usr/share/app/tree/payload.txt")
}

func TestSingleFileDestinationDirectoryKeepsBasename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "hello.txt"), "hello")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:hello.txt" = "usr/share/app/"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected package artifact, got %v", artifacts)
	}
	assertTarHas(t, artifacts[0], "usr/share/app/hello.txt")
}

func TestTarRejectsManifestOnlyMetadata(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build]
command = "printf built > built.txt"
`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
builds = ["main"]

[package]
version = "1.0.0-1"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"package"}); err == nil {
		t.Fatal("expected tar manifest metadata to fail")
	}
	if fileExists(filepath.Join(dir, "built.txt")) {
		t.Fatal("build ran before tar metadata planning error")
	}
}

func TestPublishDeduplicatesIdenticalDestination(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"

[[publish.localdir]]
path = "repo"

[[publish.localdir]]
path = "repo"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"publish"}); err != nil {
		t.Fatalf("publish failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if !fileExists(filepath.Join(dir, "repo", "main.tar")) {
		t.Fatal("publish did not copy deduped artifact")
	}
}

func TestPublishDestinationCollisionFailsBeforeCopy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, name+".package.pekit.toml"), `
format = "tar"

[package]
name = "same"

[files]
"@recipe:`+name+`.txt" = "usr/share/payload"

[[publish.localdir]]
path = "repo"
`)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"publish", "--all"}); err == nil {
		t.Fatal("expected publish destination collision to fail")
	}
	if fileExists(filepath.Join(dir, "repo", "same.tar")) {
		t.Fatal("publish copied artifact despite collision")
	}
}

func TestPublishUnanchoredURLRequiresFlagEvenDryRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.url]
url = "https://example.invalid/source.tar.gz"
extract = true
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:pekit.toml" = "usr/share/pekit.toml"

[[publish.localdir]]
path = "repo"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"publish", "--dry-run"}); err == nil {
		t.Fatal("expected unanchored publish to fail")
	}
	stdout.Reset()
	stderr.Reset()
	if err := app.Run([]string{"publish", "--dry-run", "--allow-unanchored"}); err != nil {
		t.Fatalf("allow-unanchored dry-run failed: %v", err)
	}
}

func TestURLTarXZSourceUsesURLBasenameForExtraction(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not installed")
	}
	dir := t.TempDir()
	serveDir := filepath.Join(dir, "serve")
	if err := os.MkdirAll(serveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(serveDir, "app-1.0.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	payload := []byte("payload")
	if err := tw.WriteHeader(&tar.Header{Name: "app-1.0/payload.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, serveDir, "xz", "-z", "-k", tarPath)
	rawURL := "https://example.test/app-1.0.tar.xz"
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != rawURL {
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		}
		f, err := os.Open(filepath.Join(serveDir, "app-1.0.tar.xz"))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: f, Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.xz"
extract = true
root = "app-{{version}}"

[build]
command = "cat payload.txt > \"$PEKIT_OUT/value\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "url-"+shortHash(rawURL, "", "app-1.0"), "build", "main", "value"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("unexpected build output %q", string(data))
	}
	cached, err := filepath.Glob(filepath.Join(dir, "out", "_source_cache", "url", "*", "app-1.0.tar.xz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 {
		t.Fatalf("expected cached URL artifact with extension, got %v", cached)
	}
}

func TestUnsafeZipSourceRejected(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "bad.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(zipPath, filepath.Join(dir, "extract")); err == nil {
		t.Fatal("expected unsafe archive to fail")
	}
}

func TestUnsafeTarSymlinkSourceRejected(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "bad.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(tarPath, filepath.Join(dir, "extract")); err == nil {
		t.Fatal("expected unsafe tar symlink to fail")
	}
}

func TestSafeTarSymlinkSourcePreserved(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "safe.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "link.txt", Typeflag: tar.TypeSymlink, Linkname: "file.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	extractDir := filepath.Join(dir, "extract")
	if err := extractArchive(tarPath, extractDir); err != nil {
		t.Fatalf("safe symlink archive failed: %v", err)
	}
	info, err := os.Lstat(filepath.Join(extractDir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was not preserved")
	}
	target, err := os.Readlink(filepath.Join(extractDir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "file.txt" {
		t.Fatalf("unexpected symlink target %q", target)
	}
}

// TestExtractSkipsPaxGlobalHeader: git-archive tarballs (kernel.org
// releases) open with a pax global header; it is stream metadata and must
// be skipped, not rejected as an unsupported member.
func TestExtractSkipsPaxGlobalHeader(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "gh.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc123"}}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	extractDir := filepath.Join(dir, "extract")
	if err := extractArchive(tarPath, extractDir); err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "file.txt")); err != nil {
		t.Fatalf("member after global header missing: %v", err)
	}
}

// TestExtractPreservesArchiveMtimes guards the autotools contract: release
// tarballs encode "generated outputs are newer than their inputs" in member
// mtimes, and extraction must reproduce that rather than stamping files in
// write order — write-order mtimes make maintainer rebuild rules fire in
// build environments that deliberately lack autoconf/automake.
func TestExtractPreservesArchiveMtimes(t *testing.T) {
	dir := t.TempDir()
	older := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	newer := older.Add(48 * time.Hour)
	tarPath := filepath.Join(dir, "timed.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	// The generated output is written FIRST but is the NEWER file: with
	// write-order mtimes it would end up older than its input.
	for _, entry := range []struct {
		name string
		mod  time.Time
	}{{"configure", newer}, {"configure.ac", older}} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, ModTime: entry.mod}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	extractDir := filepath.Join(dir, "extract")
	if err := extractArchive(tarPath, extractDir); err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	for name, want := range map[string]time.Time{"configure": newer, "configure.ac": older} {
		info, err := os.Stat(filepath.Join(extractDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(want) {
			t.Errorf("%s mtime = %v, want %v", name, info.ModTime(), want)
		}
	}
}

func TestArchiveRejectsExtractionThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "bad.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "real", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "real"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "alias/payload", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(tarPath, filepath.Join(dir, "extract")); err == nil {
		t.Fatal("expected extraction through symlink to fail")
	}
}

func TestWorkspaceBuildFanout(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf "$PEKIT_TARGET" > "$PEKIT_OUT/result"'
`)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "--jobs", "2", "build"}); err != nil {
		t.Fatalf("workspace build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	for _, name := range []string{"a", "b"} {
		if !fileExists(filepath.Join(dir, name, "out", "build", "main", "result")) {
			t.Fatalf("missing build output for %s", name)
		}
	}
}

func TestWorkspaceAllowUnusedSuppressesMissingTargetSelector(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "a", "pekit.toml"), `
out_dir = "out"

[build.alpha]
command = "printf alpha > \"$PEKIT_OUT/result\""
`)
	writeFile(t, filepath.Join(dir, "b", "pekit.toml"), `
out_dir = "out"

[build.beta]
command = "printf beta > \"$PEKIT_OUT/result\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "build", "alpha", "--allow-unused"}); err != nil {
		t.Fatalf("workspace build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if !fileExists(filepath.Join(dir, "a", "out", "build", "alpha", "result")) {
		t.Fatal("matching member did not build")
	}
	if dirExists(filepath.Join(dir, "b", "out")) {
		t.Fatal("non-matching member should have been skipped")
	}
}

func TestWorkspaceAllowUnusedSuppressesMissingPackageSelector(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), `out_dir = "out"`)
		writeFile(t, filepath.Join(dir, name, "payload.txt"), name)
	}
	writeFile(t, filepath.Join(dir, "a", "alpha.package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	writeFile(t, filepath.Join(dir, "b", "beta.package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "package", "alpha", "--allow-unused"}); err != nil {
		t.Fatalf("workspace package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	alpha, err := filepath.Glob(filepath.Join(dir, "a", "out", "package", "*", "alpha.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha) != 1 {
		t.Fatalf("expected alpha package, got %v", alpha)
	}
	if dirExists(filepath.Join(dir, "b", "out")) {
		t.Fatal("non-matching package member should have been skipped")
	}
}

func TestWorkspaceAllowUnusedSuppressesSourcelessAllVersions(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v1.0.0")
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "sourceless", "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "sourceless", "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "sourceless", "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	writeFile(t, filepath.Join(dir, "versioned", "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"
`)
	writeFile(t, filepath.Join(dir, "versioned", "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "versioned", "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "package", "--all-versions", "--allow-unused"}); err != nil {
		t.Fatalf("workspace package failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
}

func TestWorkspacePublishDestinationCollisionFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), `out_dir = "out"`)
		writeFile(t, filepath.Join(dir, name, "payload.txt"), name)
		writeFile(t, filepath.Join(dir, name, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/payload"

[[publish.localdir]]
path = "repo"
`)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "publish"}); err == nil {
		t.Fatal("expected workspace publish collision to fail")
	}
	if dirExists(filepath.Join(dir, "a", "out")) || dirExists(filepath.Join(dir, "b", "out")) {
		t.Fatal("workspace publish collision was not caught before member execution")
	}
}

func TestWorkspaceKeyringResolvedAtWorkspaceLevel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "prod.keyring.pekit.toml"), `token = "workspace"`)
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), `
out_dir = "out"

[build]
command = 'printf "$PEKIT_KEYRING_TOKEN" > "$PEKIT_OUT/token"'
`)
		writeFile(t, filepath.Join(dir, name, "prod.keyring.pekit.toml"), `token = "`+name+`"`)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "build", "--keyring=prod"}); err != nil {
		t.Fatalf("workspace build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	for _, name := range []string{"a", "b"} {
		data, err := os.ReadFile(filepath.Join(dir, name, "out", "build", "main", "token"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "workspace" {
			t.Fatalf("member %s used per-member keyring value %q", name, string(data))
		}
	}
}

func TestWorkspaceFailFastSkipsPendingMembers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	writeFile(t, filepath.Join(dir, "a", "pekit.toml"), `
out_dir = "out"

[build]
command = "exit 7"
`)
	writeFile(t, filepath.Join(dir, "b", "pekit.toml"), `
out_dir = "out"

[build]
command = "printf ran > \"$PEKIT_OUT/result\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"workspace", "--fail-fast", "build"}); err == nil {
		t.Fatal("expected workspace failure")
	}
	if dirExists(filepath.Join(dir, "b", "out")) {
		t.Fatal("pending member ran despite fail-fast")
	}
}

func assertTarHas(t *testing.T, path, want string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == want {
			return
		}
	}
	t.Fatalf("%s not found in %s", want, path)
}

func assertTarNotHas(t *testing.T, path, unwanted string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == unwanted {
			t.Fatalf("%s unexpectedly found in %s", unwanted, path)
		}
	}
}

type peipkgManifestDoc struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Description  string `json:"description"`
	License      string `json:"license"`
	LicenseClass string `json:"license_class"`
	Dependencies []struct {
		Name       string `json:"name"`
		Constraint string `json:"constraint"`
	} `json:"dependencies"`
	Provides []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"provides"`
	Build struct {
		FarmID        string `json:"farm_id"`
		SourceRef     string `json:"source_ref"`
		SourcePackage string `json:"source_package"`
		RecipeRef     string `json:"recipe_ref"`
		Builder       string `json:"builder"`
	} `json:"build"`
}

func readPeipkgManifest(t *testing.T, path string) peipkgManifestDoc {
	t.Helper()
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != ".peipkg/manifest.json" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		var doc peipkgManifestDoc
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	t.Fatalf(".peipkg/manifest.json not found in %s", path)
	return peipkgManifestDoc{}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func runTestCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
}

func TestURLCrateSourceExtractsAsGzippedTar(t *testing.T) {
	dir := t.TempDir()
	serveDir := filepath.Join(dir, "serve")
	if err := os.MkdirAll(serveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A .crate is cargo's publish format: a plain gzipped tarball.
	cratePath := filepath.Join(serveDir, "widget-cli-1.0.crate")
	f, err := os.Create(cratePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	payload := []byte("payload")
	if err := tw.WriteHeader(&tar.Header{Name: "widget-cli-1.0/payload.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rawURL := "https://example.test/widget-cli-1.0.crate"
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != rawURL {
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		}
		f, err := os.Open(cratePath)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: f, Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.url]
url = "https://example.test/widget-cli-{{version}}.crate"
extract = true
root = "widget-cli-{{version}}"

[build]
command = "cat payload.txt > \"$PEKIT_OUT/value\""
`)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "out", "url-"+shortHash(rawURL, "", "widget-cli-1.0"), "build", "main", "value"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("unexpected build output %q", string(data))
	}
}
