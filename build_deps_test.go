package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTargetNeeds(t *testing.T) {
	cfg, err := ParseConfig(`outDir = "out"

[build.gen]
command = "make proto"

[build.app]
needs = ["gen"]
command = "go build"
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	app := cfg.Commands["build"]["app"]
	if len(app.Needs) != 1 || app.Needs[0] != "gen" {
		t.Errorf("app.Needs = %v, want [gen]", app.Needs)
	}
	if len(cfg.Commands["build"]["gen"].Needs) != 0 {
		t.Errorf("gen should have no needs")
	}
}

func TestParseNeedsRejectsNonArray(t *testing.T) {
	if _, err := ParseConfig(`[build.app]
needs = "gen"
command = "x"
`); err == nil {
		t.Error("a string needs should error (must be an array)")
	}
}

func TestValidateDepsMissing(t *testing.T) {
	_, err := ParseConfig(`[build.app]
needs = ["gen"]
command = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "not a target") {
		t.Fatalf("err = %v, want a missing-target error", err)
	}
}

func TestValidateDepsCycle(t *testing.T) {
	_, err := ParseConfig(`[build.a]
needs = ["b"]
command = "x"

[build.b]
needs = ["a"]
command = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cycle error", err)
	}
}

func TestValidateDepsSelfCycle(t *testing.T) {
	_, err := ParseConfig(`[build.a]
needs = ["a"]
command = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a self-cycle error", err)
	}
}

func TestValidateDepsEnvCollision(t *testing.T) {
	// "a-b" and "a.b" both sanitise to PEKIT_A_B_OUT.
	_, err := ParseConfig(`[build."a-b"]
command = "x"

[build."a.b"]
command = "x"

[build.app]
needs = ["a-b", "a.b"]
command = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "PEKIT_A_B_OUT") {
		t.Fatalf("err = %v, want an env-var collision error", err)
	}
}

func TestEnvTargetName(t *testing.T) {
	cases := map[string]string{
		"gen":      "GEN",
		"libc-dev": "LIBC_DEV",
		"proto.gen": "PROTO_GEN",
		"a1":       "A1",
		"Foo_Bar":  "FOO_BAR",
	}
	for in, want := range cases {
		if got := envTargetName(in); got != want {
			t.Errorf("envTargetName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNoBuildReusesStagedElseBuilds: --no-build is a "don't rebuild" switch —
// it reuses a target whose stage already exists, but still builds one that was
// never staged. The target's command is "exit 1", so it fails loudly if it is
// actually run, letting us distinguish reuse (nil) from build (error).
func TestNoBuildReusesStagedElseBuilds(t *testing.T) {
	cfg := &Config{
		OutDir:   "out",
		ClearOut: true,
		Commands: map[string]map[string]Target{
			"build": {"gen": {Command: "exit 1"}},
		},
	}
	t.Chdir(t.TempDir())
	stage := filepath.Join("out", "build", "gen")

	// Never staged → --no-build builds it anyway (command runs, fails).
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	if err := runTarget(cfg, "build", "gen", nil, true); err == nil {
		t.Error("--no-build must build a target that was never staged")
	}

	// Already staged → --no-build reuses it (command not run).
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runTarget(cfg, "build", "gen", nil, true); err != nil {
		t.Errorf("--no-build must reuse a staged target, got %v", err)
	}

	// Already staged, no --no-build → rebuilt regardless (command runs, fails).
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runTarget(cfg, "build", "gen", nil, false); err == nil {
		t.Error("without --no-build a staged target must still rebuild")
	}
}

func TestBuildOrder(t *testing.T) {
	// Diamond: d needs b and c, both need a. a must run first and only once.
	targets := map[string]Target{
		"a": {Command: "x"},
		"b": {Command: "x", Needs: []string{"a"}},
		"c": {Command: "x", Needs: []string{"a"}},
		"d": {Command: "x", Needs: []string{"b", "c"}},
	}
	got := buildOrder(targets, "d")
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Errorf("buildOrder(d) = %v, want [a b c d]", got)
	}
	// A leaf orders to just itself.
	if got := buildOrder(targets, "a"); len(got) != 1 || got[0] != "a" {
		t.Errorf("buildOrder(a) = %v, want [a]", got)
	}
}
