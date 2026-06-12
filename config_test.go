package main

import (
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
	if cfg.Packages != nil {
		t.Errorf("want nil Packages, got %v", cfg.Packages)
	}
}

func TestParseBarePackage(t *testing.T) {
	cfg, err := ParseConfig(`
[package]
format = "peipkg"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Packages["main"].Format; got != "peipkg" {
		t.Errorf("main.Format = %q, want %q", got, "peipkg")
	}
}

func TestPackageMissingFormatRejected(t *testing.T) {
	// format is mandatory: pekit is format-agnostic, so no format gets
	// to be a silent default.
	_, err := ParseConfig(`
[package.app1]
`)
	if err == nil || !strings.Contains(err.Error(), `missing required key "format"`) {
		t.Errorf("want missing-format error, got: %v", err)
	}
}

func TestNamedPackages(t *testing.T) {
	cfg, err := ParseConfig(`
[package.app1]
format = "peipkg"

[package.app2]
format = "tar"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sortedNames(cfg.Packages); len(got) != 2 || got[0] != "app1" || got[1] != "app2" {
		t.Fatalf("package names = %v, want [app1 app2]", got)
	}
	if got := cfg.Packages["app2"].Format; got != "tar" {
		t.Errorf("app2.Format = %q, want %q", got, "tar")
	}
}

func TestMixedShapesRejectedInPackage(t *testing.T) {
	_, err := ParseConfig(`
[package]
format = "peipkg"

[package.app1]
format = "peipkg"
`)
	if err == nil || !strings.Contains(err.Error(), "[package] mixes") {
		t.Errorf("want [package] mix error, got: %v", err)
	}
}

func TestPackageUnknownKeyRejected(t *testing.T) {
	_, err := ParseConfig(`
[package]
fromat = "peipkg"
`)
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("want unknown-key error, got: %v", err)
	}
}

func TestPackageCommandKeyRejected(t *testing.T) {
	// command belongs to command verbs, not packages.
	_, err := ParseConfig(`
[package]
command = "tar czf out.tar.gz ."
`)
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("want unknown-key error for command in [package], got: %v", err)
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

func TestUnrecognisedFormatError(t *testing.T) {
	err := buildPackage("main", Package{Format: "peipkg"})
	if err == nil || !strings.Contains(err.Error(), `unrecognised package format "peipkg"`) {
		t.Errorf("want unrecognised-format error, got: %v", err)
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
