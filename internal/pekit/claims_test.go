package pekit

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPackageFileAcceptsClaims guards the gap that once rejected claims:
// parseClaims was wired up, but "claims" was missing from LoadPackageFile's
// known-key allowlist, so a real [claims] block failed the unknown-key gate
// before the handler ran. Exercise the full file parse, not parseClaims alone.
func TestLoadPackageFileAcceptsClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	const toml = `format = "peipkg"
builds = ["main"]

[package]
version = "0.0.2-1"
architecture = "x86_64"
description = "test"

[provides]
init = "1"

[claims.provides.init.bin]
target = "/usr/sbin/prelude"
path = "/init"

[files]
"out/prelude" = "usr/sbin/prelude"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadPackageFile(path)
	if err != nil {
		t.Fatalf("LoadPackageFile: %v", err)
	}
	if got := cfg.Package.Claims.Provides["init"]["bin"]; got.Target != "/usr/sbin/prelude" || got.Path != "/init" {
		t.Errorf("claim slot not parsed through LoadPackageFile: got %+v", got)
	}
}

func TestParseClaims(t *testing.T) {
	raw := map[string]any{
		"provides": map[string]any{
			"registryd": map[string]any{
				"binary": map[string]any{
					"target": "/usr/sbin/loregd", "path": "/usr/sbin/registryd"},
			},
		},
		"dependencies": map[string]any{
			"logsink": map[string]any{
				"sink": map[string]any{"path": "/run/services/logsink/logsink.sock"},
			},
		},
	}
	cm, err := parseClaims("recipe.toml", raw)
	if err != nil {
		t.Fatalf("parseClaims: %v", err)
	}
	if got := cm.Provides["registryd"]["binary"]; got.Target != "/usr/sbin/loregd" ||
		got.Path != "/usr/sbin/registryd" {
		t.Errorf("provider slot: got %+v", got)
	}
	if got := cm.Dependencies["logsink"]["sink"]; got.Path != "/run/services/logsink/logsink.sock" ||
		got.Target != "" {
		t.Errorf("consumer slot: got %+v", got)
	}
}

func TestParseClaimsRejectsBadKeys(t *testing.T) {
	cases := map[string]map[string]any{
		"unknown side": {"frobnicate": map[string]any{}},
		"unknown slot field": {"provides": map[string]any{
			"registryd": map[string]any{"binary": map[string]any{"bogus": "x"}}}},
		"non-string field": {"provides": map[string]any{
			"registryd": map[string]any{"binary": map[string]any{"target": 7}}}},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClaims("recipe.toml", raw); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestPackClaimsConversion(t *testing.T) {
	claims := ClaimsMeta{
		Provides: map[string]map[string]ClaimSlot{
			"registryd": {"binary": {Target: "/usr/sbin/loregd"}}},
		Dependencies: map[string]map[string]ClaimSlot{
			"logsink": {"sink": {Path: "/run/services/logsink/logsink.sock"}}},
	}
	prov := packProvides(map[string]string{"registryd": "1.4"}, claims.Provides)
	if len(prov) != 1 || prov[0].Claims["binary"].Target != "/usr/sbin/loregd" {
		t.Errorf("packProvides claims: got %+v", prov)
	}
	deps := packDeps(map[string]string{"logsink": "*"}, nil, claims.Dependencies)
	if len(deps) != 1 || deps[0].Claims["sink"].Path != "/run/services/logsink/logsink.sock" {
		t.Errorf("packDeps claims: got %+v", deps)
	}
	// A dependency with no claim carries a nil claims map.
	plain := packDeps(map[string]string{"libc": ">= 2.39-1"}, nil, claims.Dependencies)
	if plain[0].Claims != nil {
		t.Errorf("non-claim dep should have nil claims, got %+v", plain[0].Claims)
	}
}

// TestLoadPackageFileNamedRoots exercises the named-roots recipe surface:
// a package default_root and the string-or-table dependency form carrying
// a placement root (§3.3.6, §4.1.1, DESIGN-named-roots.md).
func TestLoadPackageFileNamedRoots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	const toml = `format = "peipkg"
builds = ["main"]

[package]
version = "1.0-1"
architecture = "x86_64"
description = "test"
default_root = "initramfs"

[dependencies]
libssl = ">= 3.0"
peiosutils = { constraint = ">= 1.0", root = "initramfs" }
busybox = { root = "initramfs.sub" }

[files]
"out/x" = "usr/bin/x"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadPackageFile(path)
	if err != nil {
		t.Fatalf("LoadPackageFile: %v", err)
	}
	if cfg.Package.DefaultRoot != "initramfs" {
		t.Errorf("default_root: got %q, want %q", cfg.Package.DefaultRoot, "initramfs")
	}
	// Short form: a constraint, no root.
	if cfg.Package.Dependencies["libssl"] != ">= 3.0" {
		t.Errorf("libssl constraint: got %q", cfg.Package.Dependencies["libssl"])
	}
	if _, placed := cfg.Package.DependencyRoots["libssl"]; placed {
		t.Errorf("libssl should carry no placement root")
	}
	// Table form: constraint + root.
	if cfg.Package.Dependencies["peiosutils"] != ">= 1.0" ||
		cfg.Package.DependencyRoots["peiosutils"] != "initramfs" {
		t.Errorf("peiosutils: constraint=%q root=%q",
			cfg.Package.Dependencies["peiosutils"], cfg.Package.DependencyRoots["peiosutils"])
	}
	// Table form with only a root: any-version, nested placement.
	if cfg.Package.Dependencies["busybox"] != "" ||
		cfg.Package.DependencyRoots["busybox"] != "initramfs.sub" {
		t.Errorf("busybox: constraint=%q root=%q",
			cfg.Package.Dependencies["busybox"], cfg.Package.DependencyRoots["busybox"])
	}
}

func TestLoadPackageFileRejectsBadRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	const toml = `format = "peipkg"
builds = ["main"]

[package]
version = "1.0-1"
architecture = "x86_64"
description = "test"

[dependencies]
peiosutils = { root = "/abs/path" }

[files]
"out/x" = "usr/bin/x"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPackageFile(path); err == nil {
		t.Error("a dependency root that is a path (not a named reference) should be rejected")
	}
}
