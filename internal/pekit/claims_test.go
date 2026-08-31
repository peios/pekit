package pekit

import (
	"os"
	"path/filepath"
	"strings"
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

// TestLoadPackageFileAlternateUpgrade exercises the [package]
// alternate_upgrade table (§5.18) in both its inline and sub-table forms,
// and the plan-time checks on its message.
func TestLoadPackageFileAlternateUpgrade(t *testing.T) {
	load := func(t *testing.T, body string) (PackageConfig, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "package.pekit.toml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadPackageFile(path)
	}
	const head = `format = "peipkg"
builds = ["main"]

[files]
"out/x" = "usr/bin/x"

[package]
version = "1.0-1"
architecture = "x86_64"
description = "test"
`
	const want = "Upgrade with upgrade-peios.\nSee the release notes."
	for name, body := range map[string]string{
		"inline":    head + "alternate_upgrade = { message = \"Upgrade with upgrade-peios.\\nSee the release notes.\" }\n",
		"sub-table": head + "\n[package.alternate_upgrade]\nmessage = \"Upgrade with upgrade-peios.\\nSee the release notes.\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := load(t, body)
			if err != nil {
				t.Fatalf("LoadPackageFile: %v", err)
			}
			if cfg.Package.AlternateUpgrade == nil || cfg.Package.AlternateUpgrade.Message != want {
				t.Errorf("alternate_upgrade: got %+v, want message %q", cfg.Package.AlternateUpgrade, want)
			}
		})
	}
	// Absent means none.
	if cfg, err := load(t, head); err != nil || cfg.Package.AlternateUpgrade != nil {
		t.Errorf("absent alternate_upgrade: cfg %+v, err %v", cfg.Package.AlternateUpgrade, err)
	}
	for name, body := range map[string]string{
		"not a table":     head + "alternate_upgrade = \"x\"\n",
		"missing message": head + "alternate_upgrade = {}\n",
		"empty message":   head + "alternate_upgrade = { message = \"\" }\n",
		"unknown key":     head + "alternate_upgrade = { message = \"x\", tool = \"y\" }\n",
		"control char":    head + "alternate_upgrade = { message = \"a\\tb\" }\n",
		"too long":        head + "alternate_upgrade = { message = \"" + strings.Repeat("x", 1025) + "\" }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, body); err == nil {
				t.Error("LoadPackageFile accepted an invalid alternate_upgrade")
			}
		})
	}
}

// TestMergePackageMetaAlternateUpgrade: an override that declares the
// table wins; one that says nothing keeps the base's.
func TestMergePackageMetaAlternateUpgrade(t *testing.T) {
	base := PackageMeta{AlternateUpgrade: &AlternateUpgradeMeta{Message: "base"}}
	if got := mergePackageMeta(base, PackageMeta{}); got.AlternateUpgrade == nil || got.AlternateUpgrade.Message != "base" {
		t.Errorf("silent override: got %+v", got.AlternateUpgrade)
	}
	over := PackageMeta{AlternateUpgrade: &AlternateUpgradeMeta{Message: "over"}}
	if got := mergePackageMeta(base, over); got.AlternateUpgrade == nil || got.AlternateUpgrade.Message != "over" {
		t.Errorf("declaring override: got %+v", got.AlternateUpgrade)
	}
	if got := mergePackageMeta(PackageMeta{}, over); got.AlternateUpgrade == nil || got.AlternateUpgrade.Message != "over" {
		t.Errorf("override onto empty base: got %+v", got.AlternateUpgrade)
	}
}

// §5.23 makes the two claim sides asymmetric: a provider names the
// target it ships, a consumer names the well-known path the role is
// reached at. peipkg enforces that at install, and pekit accepted the
// wrong shape without complaint — so the recipe author got a successful
// build, a signed package, and a failure on somebody else's machine
// (PEI-445).
func TestParseClaimsEnforcesTheSideAsymmetry(t *testing.T) {
	cases := map[string]struct {
		raw  map[string]any
		want string
	}{
		"a dependencies slot setting target": {
			raw: map[string]any{"dependencies": map[string]any{
				"registryd": map[string]any{
					"binary": map[string]any{"target": "/usr/sbin/loregd"}}}},
			want: "only a provides claim may set",
		},
		"a dependencies slot with no path": {
			raw: map[string]any{"dependencies": map[string]any{
				"registryd": map[string]any{"binary": map[string]any{}}}},
			want: "missing path",
		},
		"a provides slot with no target": {
			raw: map[string]any{"provides": map[string]any{
				"registryd": map[string]any{
					"binary": map[string]any{"path": "/usr/sbin/registryd"}}}},
			want: "missing target",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseClaims("recipe.toml", tc.raw)
			if err == nil {
				t.Fatal("the recipe was accepted and would build an uninstallable package")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}

	// A provider MAY also give a default path, which is the shape
	// TestParseClaims already covers; this is the guard that the new rule
	// has not made it invalid.
	if _, err := parseClaims("recipe.toml", map[string]any{
		"provides": map[string]any{"registryd": map[string]any{
			"binary": map[string]any{
				"target": "/usr/sbin/loregd", "path": "/usr/sbin/registryd"}}}}); err != nil {
		t.Errorf("a provider slot with target and default path was refused: %v", err)
	}
}

// A [claims.*] stanza naming a role the package neither provides nor
// depends on produced no diagnostic at all: packDeps and packProvides
// attach claims only for roles in the dependency or provides maps, so
// the claim simply did not reach the shipped manifest (PEI-445).
func TestValidateClaimRolesRejectsAStanzaForAnUnknownRole(t *testing.T) {
	meta := PackageMeta{
		Provides:             map[string]string{"registryd": ""},
		Dependencies:         map[string]string{"logsink": "*"},
		OptionalDependencies: map[string]string{"metrics": "*"},
		Claims: ClaimsMeta{
			Provides: map[string]map[string]ClaimSlot{
				"registrydd": {"binary": {Target: "/usr/sbin/loregd"}}, // typo
			},
		},
	}
	err := validateClaimRoles("pkg", meta)
	if err == nil {
		t.Fatal("a claims stanza for a role the package does not provide was accepted")
	}
	if !strings.Contains(err.Error(), "registrydd") {
		t.Errorf("error %q does not name the offending role", err)
	}

	meta.Claims.Provides = map[string]map[string]ClaimSlot{
		"registryd": {"binary": {Target: "/usr/sbin/loregd"}},
	}
	meta.Claims.Dependencies = map[string]map[string]ClaimSlot{
		"logsinkk": {"sink": {Path: "/run/x.sock"}}, // typo
	}
	if err := validateClaimRoles("pkg", meta); err == nil {
		t.Error("a claims stanza for a role the package does not depend on was accepted")
	}

	// Both dependency maps satisfy the dependencies side, since one
	// claims.dependencies map serves them both.
	meta.Claims.Dependencies = map[string]map[string]ClaimSlot{
		"logsink": {"sink": {Path: "/run/logsink.sock"}},
		"metrics": {"sink": {Path: "/run/metrics.sock"}},
	}
	if err := validateClaimRoles("pkg", meta); err != nil {
		t.Errorf("a claim on an optional dependency was refused: %v", err)
	}
}
