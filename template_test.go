package main

import (
	"strings"
	"testing"
)

func TestParseVersionFull(t *testing.T) {
	v, err := parseVersion("1.36.0-rc1+build5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Major != "1" || v.Minor != "36" || v.Patch != "0" || v.Prerelease != "rc1" || v.Buildmeta != "build5" {
		t.Errorf("parsed = %+v", v)
	}
	if v.Full != "1.36.0-rc1+build5" {
		t.Errorf("Full = %q", v.Full)
	}
}

func TestParseVersionPlain(t *testing.T) {
	v, err := parseVersion("0.34.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Patch != "0" || v.Prerelease != "" || v.Buildmeta != "" {
		t.Errorf("parsed = %+v", v)
	}
}

func TestParseVersionRejectsJunk(t *testing.T) {
	for _, s := range []string{"0.34", "v0.34.0", "1.2.3.4", "x.y.z", ""} {
		if _, err := parseVersion(s); err == nil {
			t.Errorf("parseVersion(%q) should error", s)
		}
	}
}

func TestRenderSubstitutes(t *testing.T) {
	v, _ := parseVersion("0.34.0")
	got, err := renderTemplate(`rev = "v{{version}}" ver="{{major}}.{{minor}}.{{patch}}-1"`, v)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `rev = "v0.34.0" ver="0.34.0-1"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderComponentsForUnderscoreTag(t *testing.T) {
	v, _ := parseVersion("1.36.1")
	got, _ := renderTemplate(`{{major}}_{{minor}}_{{patch}}`, v)
	if got != "1_36_1" {
		t.Errorf("got %q, want 1_36_1", got)
	}
}

func TestRenderUnknownVarErrors(t *testing.T) {
	v, _ := parseVersion("0.34.0")
	if _, err := renderTemplate(`{{majr}}`, v); err == nil || !strings.Contains(err.Error(), "unknown template variable") {
		t.Errorf("want unknown-var error, got: %v", err)
	}
}

func TestRenderTemplateWithoutVersionErrors(t *testing.T) {
	if _, err := renderTemplate(`rev = "v{{version}}"`, nil); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Errorf("want needs-version error, got: %v", err)
	}
}

func TestRenderLeavesShellBracesAlone(t *testing.T) {
	// Single braces are shell brace expansion, not templates.
	v, _ := parseVersion("0.34.0")
	in := `command = "mv foo.{txt,md} bar/ && echo {1..9}"`
	got, err := renderTemplate(in, v)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != in {
		t.Errorf("shell braces mangled: %q", got)
	}
}

func TestRenderNoVersionNoTemplatesOK(t *testing.T) {
	// A recipe with no placeholders renders fine without --version.
	in := `rev = "v0.21.0"`
	got, err := renderTemplate(in, nil)
	if err != nil || got != in {
		t.Errorf("got %q err %v", got, err)
	}
}

func TestExtractVersionForms(t *testing.T) {
	for _, args := range [][]string{
		{"build", "-V", "0.34.0"},
		{"build", "--version", "0.34.0"},
		{"build", "--version=0.34.0"},
	} {
		rest, v, err := extractVersion(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if v == nil || v.Full != "0.34.0" {
			t.Errorf("%v: version = %+v", args, v)
		}
		if len(rest) != 1 || rest[0] != "build" {
			t.Errorf("%v: rest = %v", args, rest)
		}
	}
	rest, v, err := extractVersion([]string{"build"})
	if err != nil || v != nil || len(rest) != 1 {
		t.Errorf("no-flag: rest=%v v=%v err=%v", rest, v, err)
	}
}
