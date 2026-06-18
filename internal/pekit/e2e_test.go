package pekit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
version = "1.0.0"
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
	artifacts, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "main_1.0.0_x86_64.peipkg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected peipkg artifact, got %v", artifacts)
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
version = "1.0.0"

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
	runTestCmd(t, repo, "git", "tag", "v1.0.0")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload2")
	runTestCmd(t, repo, "git", "commit", "-am", "second")
	runTestCmd(t, repo, "git", "tag", "v1.2.0")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"
tag_regex = '^v[0-9]+\.[0-9]+\.[0-9]+$'

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
	if string(data) != "1.2.0" {
		t.Fatalf("latest version = %q", string(data))
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
version = "1.0.0"

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

func runTestCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
}
