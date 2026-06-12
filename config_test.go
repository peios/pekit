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
	if len(cfg.Targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(cfg.Targets))
	}
	if got := cfg.Targets["main"].Command; got != "cargo build" {
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
	if got := cfg.TargetNames(); len(got) != 2 || got[0] != "app1" || got[1] != "app2" {
		t.Fatalf("TargetNames() = %v, want [app1 app2]", got)
	}
	if got := cfg.Targets["app2"].Command; got != "cargo build -p app2" {
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

func TestMissingBuildSection(t *testing.T) {
	_, err := ParseConfig(``)
	if err == nil || !strings.Contains(err.Error(), "missing [build]") {
		t.Errorf("want missing-[build] error, got: %v", err)
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
[build]
command = 42
`)
	if err == nil || !strings.Contains(err.Error(), "string") {
		t.Errorf("want non-string-command error, got: %v", err)
	}
}
