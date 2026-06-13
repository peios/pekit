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
		rest, f, err := extractFlags(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !f.hasVersion || f.version != "0.34.0" {
			t.Errorf("%v: version=%q has=%v", args, f.version, f.hasVersion)
		}
		if len(rest) != 1 || rest[0] != "build" {
			t.Errorf("%v: rest = %v", args, rest)
		}
	}
	rest, f, err := extractFlags([]string{"build"})
	if err != nil || f.hasVersion || len(rest) != 1 {
		t.Errorf("no-flag: rest=%v has=%v err=%v", rest, f.hasVersion, err)
	}
	rest, f, err = extractFlags([]string{"package", "--version=*", "--remember-built", "--bust"})
	if err != nil || !f.remember || !f.bust || f.version != "*" || len(rest) != 1 || rest[0] != "package" {
		t.Errorf("flags: rest=%v f=%+v err=%v", rest, f, err)
	}
}

func TestResolveVersionsMultiple(t *testing.T) {
	vers, err := resolveVersions("0.33.0, 0.34.0,0.34.0", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vers) != 2 || vers[0].Full != "0.33.0" || vers[1].Full != "0.34.0" {
		t.Errorf("vers = %v (want [0.33.0 0.34.0], dups dropped, order kept)", vers)
	}
}

func TestResolveVersionsAbsentIsSingleNil(t *testing.T) {
	vers, err := resolveVersions("", false)
	if err != nil || len(vers) != 1 || vers[0] != nil {
		t.Errorf("absent: vers=%v err=%v", vers, err)
	}
}

func TestResolveVersionsRejectsJunkMember(t *testing.T) {
	if _, err := resolveVersions("0.34.0,nope", true); err == nil {
		t.Error("want error for junk version in list")
	}
}
