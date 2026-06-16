package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverPackages(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("format=\"tar\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Members (sorted out of order to prove sorting), the shared base, an
	// unrelated file, a dotfile-prefixed match, and a same-named directory —
	// only the two real members should come back.
	write("locales.package.pekit.toml")
	write("libc.package.pekit.toml")
	write("package.pekit.toml")  // the base, excluded
	write("pekit.toml")          // unrelated
	write(".package.pekit.toml") // empty prefix, excluded
	if err := os.Mkdir(filepath.Join(dir, "sub.package.pekit.toml"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := discoverPackages(dir)
	if err != nil {
		t.Fatalf("discoverPackages: %v", err)
	}
	if strings.Join(memberNames(got), ",") != "libc,locales" {
		t.Errorf("members = %v, want [libc locales]", memberNames(got))
	}
}

func TestDiscoverPackagesSubdir(t *testing.T) {
	dir := t.TempDir()
	mk := func(p string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("format=\"tar\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Members come from the recipe dir AND the package(s).pekit subdirs.
	mk("libc.package.pekit.toml")               // recipe root
	mk("package.pekit/zlib.package.pekit.toml") // package.pekit/
	mk("packages.pekit/gmp.package.pekit.toml") // packages.pekit/
	mk("package.pekit/package.pekit.toml")      // bare base in subdir, excluded

	got, err := discoverPackages(dir)
	if err != nil {
		t.Fatalf("discoverPackages: %v", err)
	}
	if strings.Join(memberNames(got), ",") != "gmp,libc,zlib" {
		t.Errorf("members = %v, want [gmp libc zlib]", memberNames(got))
	}
	// Each member's path points at where it was found.
	for _, m := range got {
		if m.name == "zlib" && filepath.Base(filepath.Dir(m.path)) != "package.pekit" {
			t.Errorf("zlib path = %s, want it under package.pekit/", m.path)
		}
	}
}

func TestDiscoverPackagesDuplicate(t *testing.T) {
	dir := t.TempDir()
	mk := func(p string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("format=\"tar\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("libc.package.pekit.toml")
	mk("packages.pekit/libc.package.pekit.toml")
	if _, err := discoverPackages(dir); err == nil || !strings.Contains(err.Error(), "duplicate package") {
		t.Fatalf("err = %v, want a duplicate-package error", err)
	}
}

func TestDiscoverPackagesNone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.pekit.toml"), []byte("format=\"tar\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := discoverPackages(dir)
	if err != nil {
		t.Fatalf("discoverPackages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a lone base is not a member; got %v", got)
	}
}

func TestRawPackageName(t *testing.T) {
	cases := []struct {
		raw  map[string]any
		want string
	}{
		{map[string]any{"package": map[string]any{"name": "libc"}}, "libc"},
		{map[string]any{"package": map[string]any{"license": "LGPL"}}, ""}, // no name
		{map[string]any{"format": "tar"}, ""},                              // no [package]
		{nil, ""},
	}
	for _, c := range cases {
		if got := rawPackageName(c.raw); got != c.want {
			t.Errorf("rawPackageName(%v) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestExtractFlagsNoBuild(t *testing.T) {
	rest, f, err := extractFlags([]string{"package", "--no-build"})
	if err != nil {
		t.Fatalf("extractFlags: %v", err)
	}
	if !f.noBuild.active || !f.noBuild.all {
		t.Error("bare --no-build should select all targets")
	}
	if len(rest) != 1 || rest[0] != "package" {
		t.Errorf("rest = %v, want [package] (the flag is consumed, not positional)", rest)
	}

	// --no-build=a,b selects only the named targets.
	_, f, err = extractFlags([]string{"build", "--no-build=upstream, gen"})
	if err != nil {
		t.Fatalf("extractFlags: %v", err)
	}
	if !f.noBuild.active || f.noBuild.all {
		t.Error("--no-build=names should be active but not all")
	}
	if !f.noBuild.names["upstream"] || !f.noBuild.names["gen"] {
		t.Errorf("--no-build=names parsed %v, want {upstream, gen}", f.noBuild.names)
	}

	// --no-build= (empty value) is an error.
	if _, _, err := extractFlags([]string{"build", "--no-build="}); err == nil {
		t.Error("--no-build= with an empty value should error")
	}
}

func TestExtractFlagsUnknownFlagErrors(t *testing.T) {
	// The bug that motivated this: an unknown --flag must error, not fall
	// through to be misread as a target or package name.
	if _, _, err := extractFlags([]string{"package", "--no-buidl"}); err == nil {
		t.Error("an unknown flag should error")
	}
	// A bare (non-dash) positional is still a positional.
	rest, _, err := extractFlags([]string{"package", "libc"})
	if err != nil || len(rest) != 2 || rest[1] != "libc" {
		t.Errorf("positional: rest=%v err=%v", rest, err)
	}
}

func TestPackageSelector(t *testing.T) {
	if s, err := packageSelector(nil); err != nil || s != "" {
		t.Errorf("no args = (%q,%v), want (\"\",nil)", s, err)
	}
	if s, err := packageSelector([]string{"libc"}); err != nil || s != "libc" {
		t.Errorf("one arg = (%q,%v), want (libc,nil)", s, err)
	}
	if _, err := packageSelector([]string{"a", "b"}); err == nil {
		t.Error("two args should be a usage error")
	}
}

// TestBuildPackagesUnknownName drives buildPackages far enough to hit the
// selection error: a name that no member file provides is rejected (before
// any source fetch) with the available members listed.
func TestBuildPackagesUnknownName(t *testing.T) {
	dir := t.TempDir()
	for _, m := range []string{"libc.package.pekit.toml", "libc-dev.package.pekit.toml"} {
		if err := os.WriteFile(filepath.Join(dir, m), []byte("format=\"tar\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	_, _, err := buildPackages("locales", nil, false, noBuildSet{}, "none", nil)
	if err == nil || !strings.Contains(err.Error(), "no such package") {
		t.Fatalf("err = %v, want a 'no such package' error", err)
	}
	if !strings.Contains(err.Error(), "libc") || !strings.Contains(err.Error(), "libc-dev") {
		t.Errorf("err %q should list available members", err)
	}
}

// TestBuildPackagesNoNamedPackages: asking for a named package in a recipe
// that has only the bare package.pekit.toml is an error, not a silent build
// of the standalone package.
func TestBuildPackagesNoNamedPackages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.pekit.toml"), []byte("format=\"tar\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	_, _, err := buildPackages("libc", nil, false, noBuildSet{}, "none", nil)
	if err == nil || !strings.Contains(err.Error(), "no named packages") {
		t.Fatalf("err = %v, want a 'no named packages' error", err)
	}
}

// TestPackageBaseMemberMerge proves the layering packOne relies on: a member's
// prefixed file overlays the shared base — [package] merges field-by-field
// (member wins, base fills), every other section is whole-unit (a member's
// [files] stands alone, format inherits from the base).
func TestPackageBaseMemberMerge(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "package.pekit.toml")
	if err := os.WriteFile(base, []byte(`format = "tar"

[package]
name = "glibc"
license = "LGPL-2.1-or-later"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(dir, "libc-dev.package.pekit.toml")
	if err := os.WriteFile(member, []byte(`[package]
description = "development headers"

[files]
"main:usr/include" = "usr/include"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ver, err := parseVersion("2.43")
	if err != nil {
		t.Fatal(err)
	}
	baseRaw, _, err := decodePackageFile(base, ver)
	if err != nil {
		t.Fatal(err)
	}
	memRaw, _, err := decodePackageFile(member, ver)
	if err != nil {
		t.Fatal(err)
	}

	pf, err := parsePackageRaw(mergePackageRaw(memRaw, baseRaw))
	if err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	if pf.Format != "tar" {
		t.Errorf("format = %q, want tar (inherited from base)", pf.Format)
	}
	if pf.License != "LGPL-2.1-or-later" {
		t.Errorf("license = %q, want the base value (field-merge fills it)", pf.License)
	}
	if pf.Description != "development headers" {
		t.Errorf("description = %q, want the member value", pf.Description)
	}
	if len(pf.Files) != 1 || pf.Files[0].Dest != "usr/include" {
		t.Errorf("files = %v, want the member's single mapping", pf.Files)
	}
	// The member sets no name, so packOne would fall back to its filename
	// prefix; the base's name never leaks across members.
	if got := rawPackageName(memRaw); got != "" {
		t.Errorf("member rawPackageName = %q, want \"\" (so the prefix wins)", got)
	}
}
