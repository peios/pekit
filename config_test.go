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
	build := cfg.Sections["build"]
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
	build := cfg.Sections["build"]
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
	if got := cfg.Sections["build"]["main"].Command; got != "go build ./..." {
		t.Errorf("build main.Command = %q", got)
	}
	if got := cfg.Sections["install"]["app1"].Command; got != "go install ./cmd/app1" {
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
	if len(cfg.Sections) != 0 {
		t.Errorf("want no sections, got %v", cfg.Sections)
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
