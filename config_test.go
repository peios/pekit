package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseBareBuild(t *testing.T) {
	cfg, err := ParseConfig(`
[build]
command = "cargo build"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	build := cfg.Commands["build"]
	if len(build) != 1 {
		t.Fatalf("want 1 build target, got %d", len(build))
	}
	if got := build["main"].Command; got != "cargo build" {
		t.Errorf("main.Command = %q, want %q", got, "cargo build")
	}
}

func TestParseNamedTargets(t *testing.T) {
	cfg, err := ParseConfig(`
[build.app1]
command = "cargo build -p app1"

[build.app2]
command = "cargo build -p app2"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	build := cfg.Commands["build"]
	if got := sortedNames(build); len(got) != 2 || got[0] != "app1" || got[1] != "app2" {
		t.Fatalf("target names = %v, want [app1 app2]", got)
	}
	if got := build["app2"].Command; got != "cargo build -p app2" {
		t.Errorf("app2.Command = %q", got)
	}
}

func TestMixedShapesRejected(t *testing.T) {
	_, err := ParseConfig(`
[build]
command = "cargo build"

[build.app1]
command = "cargo build -p app1"
`)
	if err == nil {
		t.Fatal("want error for mixed bare/named [build], got nil")
	}
	if !strings.Contains(err.Error(), "mixes") {
		t.Errorf("error should mention mixing, got: %v", err)
	}
}

func TestMixedShapesRejectedInInstall(t *testing.T) {
	_, err := ParseConfig(`
[install]
command = "go install ."

[install.app1]
command = "go install ./cmd/app1"
`)
	if err == nil || !strings.Contains(err.Error(), "[install] mixes") {
		t.Errorf("want [install] mix error, got: %v", err)
	}
}

func TestSectionsHaveIndependentShapes(t *testing.T) {
	cfg, err := ParseConfig(`
[build]
command = "go build ./..."

[install.app1]
command = "go install ./cmd/app1"
`)
	if err != nil {
		t.Fatalf("bare [build] alongside named [install.*] should parse, got: %v", err)
	}
	if got := cfg.Commands["build"]["main"].Command; got != "go build ./..." {
		t.Errorf("build main.Command = %q", got)
	}
	if got := cfg.Commands["install"]["app1"].Command; got != "go install ./cmd/app1" {
		t.Errorf("install app1.Command = %q", got)
	}
}

func TestMissingSectionsAreValid(t *testing.T) {
	// Sections are optional at parse time; "no such section" is an
	// invocation-time error so e.g. a build-only project can exist.
	cfg, err := ParseConfig(``)
	if err != nil {
		t.Fatalf("empty config should parse, got: %v", err)
	}
	if len(cfg.Commands) != 0 {
		t.Errorf("want no command sections, got %v", cfg.Commands)
	}
}







func TestUnknownSectionRejected(t *testing.T) {
	_, err := ParseConfig(`
[buidl]
command = "cargo build"
`)
	if err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Errorf("want unknown-section error, got: %v", err)
	}
}

func TestUnknownTargetKeyRejected(t *testing.T) {
	_, err := ParseConfig(`
[build.app1]
command = "cargo build"
comand = "typo"
`)
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("want unknown-key error, got: %v", err)
	}
}

func TestMissingCommandRejected(t *testing.T) {
	_, err := ParseConfig(`
[build.app1]
`)
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Errorf("want missing-command error, got: %v", err)
	}
}

func TestEmptyCommandRejected(t *testing.T) {
	_, err := ParseConfig(`
[build]
command = ""
`)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want empty-command error, got: %v", err)
	}
}

func TestNonStringCommandRejected(t *testing.T) {
	_, err := ParseConfig(`
[install]
command = 42
`)
	if err == nil || !strings.Contains(err.Error(), "string") {
		t.Errorf("want non-string-command error, got: %v", err)
	}
}



func TestOutDirAndClearOutParsed(t *testing.T) {
	cfg, err := ParseConfig(`
outDir = "out"
clearOut = false

[build]
command = "go build -o $PEKIT_OUT/x ."
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OutDir != "out" {
		t.Errorf("OutDir = %q, want %q", cfg.OutDir, "out")
	}
	if cfg.ClearOut {
		t.Error("ClearOut = true, want false")
	}
}

func TestClearOutDefaultsTrue(t *testing.T) {
	cfg, err := ParseConfig(`
outDir = "out"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ClearOut {
		t.Error("ClearOut should default to true")
	}
}

func TestClearOutWithoutOutDirRejected(t *testing.T) {
	_, err := ParseConfig(`
clearOut = false
`)
	if err == nil || !strings.Contains(err.Error(), "requires outDir") {
		t.Errorf("want clearOut-requires-outDir error, got: %v", err)
	}
}

func TestOutDirMustBeString(t *testing.T) {
	_, err := ParseConfig(`
outDir = 42
`)
	if err == nil || !strings.Contains(err.Error(), "outDir must be") {
		t.Errorf("want outDir type error, got: %v", err)
	}
}

func TestClearOutMustBeBool(t *testing.T) {
	_, err := ParseConfig(`
outDir = "out"
clearOut = "yes"
`)
	if err == nil || !strings.Contains(err.Error(), "clearOut must be") {
		t.Errorf("want clearOut type error, got: %v", err)
	}
}

func TestUnknownRootKeyRejected(t *testing.T) {
	_, err := ParseConfig(`
outdir = "out"
`)
	if err == nil || !strings.Contains(err.Error(), `unknown root key "outdir"`) {
		t.Errorf("want unknown-root-key error, got: %v", err)
	}
}

func TestScalarSectionRejected(t *testing.T) {
	_, err := ParseConfig(`
build = "go build ./..."
`)
	if err == nil || !strings.Contains(err.Error(), "[build] must be a table") {
		t.Errorf("want section-must-be-table error, got: %v", err)
	}
}

func TestTestVerbParsed(t *testing.T) {
	cfg, err := ParseConfig(`
[test]
command = "cargo test"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Commands["test"]["main"].Command; got != "cargo test" {
		t.Errorf("test main.Command = %q", got)
	}
}

func TestEnvParsedInDocumentOrder(t *testing.T) {
	cfg, err := ParseConfig(`
[env]
ZED = "z"
TC = "$HOME/.rustup/bin"
PATH = "$TC:$PATH"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []EnvVar{
		{"ZED", "z"},
		{"TC", "$HOME/.rustup/bin"},
		{"PATH", "$TC:$PATH"},
	}
	if len(cfg.Env) != len(want) {
		t.Fatalf("Env = %v, want %v", cfg.Env, want)
	}
	for i := range want {
		if cfg.Env[i] != want[i] {
			t.Fatalf("Env[%d] = %v, want %v (document order)", i, cfg.Env[i], want[i])
		}
	}
}

func TestEnvInvalidNameRejected(t *testing.T) {
	_, err := ParseConfig(`
[env]
"BAD-NAME" = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "invalid variable name") {
		t.Errorf("want invalid-name error, got: %v", err)
	}
}

func TestEnvInjectionNameRejected(t *testing.T) {
	_, err := ParseConfig(`
[env]
"X\"; rm -rf /; \"" = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "invalid variable name") {
		t.Errorf("want invalid-name error, got: %v", err)
	}
}

func TestEnvPekitOutRejected(t *testing.T) {
	_, err := ParseConfig(`
[env]
PEKIT_OUT = "/tmp/elsewhere"
`)
	if err == nil || !strings.Contains(err.Error(), "PEKIT_OUT") {
		t.Errorf("want PEKIT_OUT-reserved error, got: %v", err)
	}
}

func TestEnvNonStringValueRejected(t *testing.T) {
	_, err := ParseConfig(`
[env]
RETRIES = 3
`)
	if err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("want non-string error, got: %v", err)
	}
}

func TestEnvTableRejected(t *testing.T) {
	_, err := ParseConfig(`
[env.build]
CC = "gcc"
`)
	if err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("want table-rejected error, got: %v", err)
	}
}

func TestEnvPrelude(t *testing.T) {
	got := envPrelude([]EnvVar{{"TC", "$HOME/bin"}, {"PATH", "$TC:$PATH"}})
	want := "export TC=\"$HOME/bin\"\nexport PATH=\"$TC:$PATH\"\n"
	if got != want {
		t.Errorf("envPrelude = %q, want %q", got, want)
	}
}

func TestCleanVerbParsed(t *testing.T) {
	cfg, err := ParseConfig(`
[clean]
command = "cargo clean"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Commands["clean"]["main"].Command; got != "cargo clean" {
		t.Errorf("clean main.Command = %q", got)
	}
}

func TestDangerousOutDirRejected(t *testing.T) {
	// pekit clean RemoveAlls outDir; these values would delete the
	// project or worse.
	for _, dir := range []string{".", "..", "/", "./", "foo/.."} {
		_, err := ParseConfig("outDir = \"" + dir + "\"\n")
		if err == nil || !strings.Contains(err.Error(), "subdirectory") {
			t.Errorf("outDir %q: want subdirectory error, got: %v", dir, err)
		}
	}
}

func TestPackageSectionMovedError(t *testing.T) {
	_, err := ParseConfig(`
[package]
format = "tar"
`)
	if err == nil || !strings.Contains(err.Error(), "moved to package.pekit.toml") {
		t.Errorf("want moved-to-package-file error, got: %v", err)
	}
}

func TestSourceParsed(t *testing.T) {
	cfg, err := ParseConfig(`
outDir = "out"

[source]
git = "https://github.com/mirror/busybox.git"
rev = "1_36_1"

[build]
command = "make"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Source == nil || cfg.Source.Git != "https://github.com/mirror/busybox.git" || cfg.Source.Rev != "1_36_1" {
		t.Errorf("Source = %+v", cfg.Source)
	}
}

func TestSourceRequiresGitAndRev(t *testing.T) {
	_, err := ParseConfig("outDir=\"out\"\n[source]\nrev=\"x\"\n")
	if err == nil || !strings.Contains(err.Error(), `missing required key "git"`) {
		t.Errorf("want missing-git error, got: %v", err)
	}
	_, err = ParseConfig("outDir=\"out\"\n[source]\ngit=\"u\"\n")
	if err == nil || !strings.Contains(err.Error(), `missing required key "rev"`) {
		t.Errorf("want missing-rev error, got: %v", err)
	}
}

func TestSourceRequiresOutDir(t *testing.T) {
	_, err := ParseConfig(`
[source]
git = "u"
rev = "r"
`)
	if err == nil || !strings.Contains(err.Error(), "[source] requires outDir") {
		t.Errorf("want source-requires-outDir error, got: %v", err)
	}
}

func TestSourceUnknownKeyRejected(t *testing.T) {
	_, err := ParseConfig("outDir=\"out\"\n[source]\ngit=\"u\"\nrev=\"r\"\nbranch=\"main\"\n")
	if err == nil || !strings.Contains(err.Error(), `[source]: unknown key "branch"`) {
		t.Errorf("want unknown-key error, got: %v", err)
	}
}

func TestSourceDirFlattensRev(t *testing.T) {
	got := sourceDir(&Source{Git: "u", Rev: "refs/tags/v1.2"}, "out")
	if got != "out/source/refs_tags_v1.2" {
		t.Errorf("sourceDir = %q, want flattened", got)
	}
}

func TestRemoteSourceExampleParses(t *testing.T) {
	data, err := os.ReadFile("examples/remote-source/pekit.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(string(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Source == nil || cfg.Source.Rev != "1_36_1" {
		t.Errorf("Source = %+v", cfg.Source)
	}
}

func TestLoregdRecipeExampleParses(t *testing.T) {
	data, err := os.ReadFile("examples/loregd/pekit.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(string(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Source == nil || cfg.Source.Git != "https://github.com/peios/loregd.git" || cfg.Source.Rev != "v0.21.0" {
		t.Errorf("Source = %+v", cfg.Source)
	}
}

func TestDefaultNameFromGitURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/peios/loregd.git": "loregd",
		"https://github.com/peios/loregd":     "loregd",
		"git@github.com:peios/loregd.git":     "loregd",
		"/home/jack/projects/peios/loregd":    "loregd",
		"https://example.com/x/loregd.git/":   "loregd",
	}
	for url, want := range cases {
		if got := defaultName(&Source{Git: url, Rev: "x"}, "/tmp/recipe-dir"); got != want {
			t.Errorf("defaultName(%q) = %q, want %q", url, got, want)
		}
	}
	if got := defaultName(nil, "/tmp/foo/bar"); got != "bar" {
		t.Errorf("defaultName(nil) = %q, want bar", got)
	}
}

func TestCoolerRecipeParses(t *testing.T) {
	// A pure-delegate recipe: just [source], no build/package — pekit
	// borrows both from the fetched source.
	data, err := os.ReadFile("examples/loregd-cooler/pekit.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(string(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Source == nil {
		t.Fatal("expected [source]")
	}
	if _, hasBuild := cfg.Commands["build"]; hasBuild {
		t.Error("cooler recipe should have no [build] of its own")
	}
}

func TestActaRecipeExampleParses(t *testing.T) {
	data, err := os.ReadFile("examples/acta/pekit.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(string(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Source == nil || cfg.Source.Rev != "v0.34.0" {
		t.Errorf("Source = %+v", cfg.Source)
	}
}
