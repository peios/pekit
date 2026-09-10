package pekit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRecipeStrictV2Shape(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"
delegate = true

[env]
CC = "clang"

[source.git]
url = "https://example.invalid/repo.git"
ref = "v{{version}}"

[source.local]
path = "../src"

[build.main]
command = "make"
needs = ["prep"]

[build.main.dependencies.peipkg]
gmp-devel = ">= 6.3.0"

[build.main.dependencies.apt]
"libgmp-dev" = ">= 2:6.3.0"
"g++" = "*"

[build.prep]
command = ["true"]
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatalf("load recipe: %v", err)
	}
	if recipe.OutDir != "out" || !recipe.Delegate.AllowsBuild() || recipe.Source.Git.URL == "" {
		t.Fatalf("unexpected recipe: %#v", recipe)
	}
	if recipe.Targets[CommandBuild]["main"].Needs[0] != "prep" {
		t.Fatalf("target needs not decoded: %#v", recipe.Targets[CommandBuild]["main"])
	}
	if recipe.Targets[CommandBuild]["main"].Dependencies["apt"]["g++"] != "*" {
		t.Fatalf("target dependencies not decoded: %#v", recipe.Targets[CommandBuild]["main"].Dependencies)
	}
}

func TestLoadRecipeRejectsCamelCaseOutDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `outDir = "out"`)
	_, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err == nil {
		t.Fatal("expected outDir to be rejected in v2")
	}
}

func TestLoadRecipeRejectsOutDirOutsideRecipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	for _, out := range []string{"", ".", "..", filepath.Dir(dir)} {
		writeFile(t, path, "out_dir = \""+out+"\"\n")
		_, err := LoadRecipe(path)
		if err == nil || diagCode(err) != "invalid_path" {
			t.Fatalf("out_dir %q: expected invalid_path, got %v", out, err)
		}
	}
}

func TestLoadRecipeAllowsOutDirBelowRecipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pekit.toml")
	for _, out := range []string{"out", "build/out", filepath.Join(dir, "absolute-out")} {
		writeFile(t, path, "out_dir = \""+out+"\"\n")
		if _, err := LoadRecipe(path); err != nil {
			t.Fatalf("out_dir %q: unexpected error: %v", out, err)
		}
	}
}

func TestLoadRecipePyPISource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.pypi]
project = "AsciiDoc"
artifact = "sdist"
versions = ">= 10.2.1"
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatalf("load recipe: %v", err)
	}
	if recipe.Source.PyPI.Project != "AsciiDoc" || recipe.Source.PyPI.Artifact != "sdist" || recipe.Source.PyPI.Versions != ">= 10.2.1" {
		t.Fatalf("unexpected PyPI source: %#v", recipe.Source.PyPI)
	}
}

func TestLoadRecipeURLListingURL(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.url]
url = "https://example.invalid/download/v{{version}}/demo-{{version}}.tar.gz"
listing_url = "https://example.invalid/releases"
file_regex = 'v[0-9]+\\.[0-9]+\\.[0-9]+'
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatalf("load recipe: %v", err)
	}
	if got := recipe.Source.URL.ListingURL; got != "https://example.invalid/releases" {
		t.Fatalf("listing_url = %q", got)
	}
}

func TestLoadRecipeURLSignatureIgnoreExpiry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.url]
url = "https://example.invalid/demo.tar.gz"

[source.url.signature]
key_files = ["keys/upstream.asc"]
ignore_expiry = true
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatalf("load recipe: %v", err)
	}
	if !recipe.Source.URL.Signature.IgnoreExpiry {
		t.Fatal("ignore_expiry was not decoded")
	}
}

func TestLoadRecipeURLSignatureRejectsNonBooleanIgnoreExpiry(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.url]
url = "https://example.invalid/demo.tar.gz"

[source.url.signature]
key_files = ["keys/upstream.asc"]
ignore_expiry = "true"
`)
	if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
		t.Fatal("expected non-boolean ignore_expiry to fail")
	}
}

func TestLoadRecipeTrackedGitSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.git]
url = "https://example.invalid/repo.git"
ref = "refs/heads/release"
tracked_path = "security/trust/certdata.txt"
versions = ">= 2026.01.01"
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatalf("load recipe: %v", err)
	}
	if got := recipe.Source.Git.TrackedPath; got != "security/trust/certdata.txt" {
		t.Fatalf("tracked_path = %q", got)
	}
}

func TestLoadRecipeRejectsUnsafeOrTemplatedTrackedGitSource(t *testing.T) {
	for name, source := range map[string]string{
		"escape":    "ref = \"release\"\ntracked_path = \"../certdata.txt\"",
		"directory": "ref = \"release\"\ntracked_path = \".\"",
		"template":  "ref = \"v{{version}}\"\ntracked_path = \"certdata.txt\"",
		"tags":      "ref = \"release\"\ntracked_path = \"certdata.txt\"\ntag_regex = \".*\"",
		"refspec":   "ref = \"release:refs/heads/other\"\ntracked_path = \"certdata.txt\"",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "pekit.toml"), "[source.git]\nurl = \"https://example.invalid/repo.git\"\n"+source+"\n")
			if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
				t.Fatal("expected invalid tracked git source to fail")
			}
		})
	}
}

func TestLoadRecipeRejectsInvalidPyPISource(t *testing.T) {
	for name, source := range map[string]string{
		"missing artifact": `project = "demo"`,
		"unknown artifact": `project = "demo"
artifact = "wheel"`,
		"invalid project": `project = "-demo"
artifact = "sdist"`,
		"unknown key": `project = "demo"
artifact = "sdist"
index = "https://example.test/simple/"`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "pekit.toml"), "[source.pypi]\n"+source+"\n")
			if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
				t.Fatal("expected invalid PyPI source to fail")
			}
		})
	}
}

func TestLoadRecipeRejectsMixedPyPIAndURLSources(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.pypi]
project = "demo"
artifact = "sdist"

[source.url]
url = "https://example.test/demo.tar.gz"
`)
	if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
		t.Fatal("expected mixed reproducible sources to fail")
	}
}

func TestLoadPackageFileEntryOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	writeFile(t, path, `
format = "tar"

[package]
version = "1.0.0"

[files]
"@recipe:file.txt" = { path = "lib/file.txt", override = true }
`)
	cfg, err := LoadPackageFile(path)
	if err != nil {
		t.Fatalf("load package: %v", err)
	}
	entry := cfg.Files["@recipe:file.txt"]
	if entry.Path != "lib/file.txt" || !entry.Override {
		t.Fatalf("unexpected file entry: %#v", entry)
	}
}

func TestLoadPackageSymlinkEntryOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	writeFile(t, path, `
format = "peipkg"

[symlinks]
"lib/libspecial.so" = { target = "libspecial.so.1", override = true }
`)
	cfg, err := LoadPackageFile(path)
	if err != nil {
		t.Fatalf("load package: %v", err)
	}
	entry := cfg.Symlinks["lib/libspecial.so"]
	if entry.Target != "libspecial.so.1" || !entry.Override {
		t.Fatalf("unexpected symlink entry: %#v", entry)
	}
}

func TestWrapCommandRequiresPlaceholder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.pekit.toml")
	writeFile(t, path, `
[wrap]
command = ["sh", "-euc", "missing"]
`)
	if _, err := LoadEnvFile(path, false); err == nil {
		t.Fatal("expected wrapper without {{command}} to fail")
	}
}

func TestEnvFileRequiresEnvOrWrap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.pekit.toml")
	writeFile(t, path, ``)
	if _, err := LoadEnvFile(path, false); err == nil {
		t.Fatal("expected empty env file to fail")
	}
}

func TestEnvFileAllowsDependencyProviderOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apt.env.pekit.toml")
	writeFile(t, path, `dependency_provider = "apt"`)
	env, err := LoadEnvFile(path, false)
	if err != nil {
		t.Fatalf("load env file: %v", err)
	}
	if env.DependencyProvider != "apt" {
		t.Fatalf("dependency provider = %q", env.DependencyProvider)
	}
}

func TestWorkspaceRequiresInclude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.pekit.toml")
	writeFile(t, path, `[env]
CC = "clang"
`)
	if _, err := LoadWorkspace(path); err == nil {
		t.Fatal("expected workspace without include to fail")
	}
}

func TestWorkspaceSymbolVersionPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.pekit.toml")
	writeFile(t, path, `include = ["**/package.pekit.toml"]

[policy.symbol_versions]
"libc.so.6" = "GLIBC_"
"libgcc_s.so.1" = "GCC_"
`)
	cfg, err := LoadWorkspace(path)
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if got := cfg.Policy.SymbolVersions["libc.so.6"]; got != "GLIBC_" {
		t.Errorf("libc.so.6 prefix = %q, want GLIBC_", got)
	}
	pol := cfg.symbolVersionPolicy()
	if pol["libgcc_s.so.1"] != "GCC_" {
		t.Errorf("symbolVersionPolicy() = %v", pol)
	}
	// A nil/empty workspace yields a nil policy, not a panic.
	if (*WorkspaceConfig)(nil).symbolVersionPolicy() != nil {
		t.Error("nil workspace should yield nil policy")
	}
}

func TestWorkspaceRejectsUnknownPolicyTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.pekit.toml")
	writeFile(t, path, `include = ["**/package.pekit.toml"]

[policy.bogus]
x = "y"
`)
	if _, err := LoadWorkspace(path); err == nil {
		t.Fatal("expected unknown policy sub-table to fail")
	}
}

func TestEnvNamesMustBeShellVariables(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[env]
"bad-name" = "nope"
`)
	if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
		t.Fatal("expected invalid env name to fail")
	}
}

func TestEnvPreservesDocumentOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[env]
Z_BASE = "base"
A_CHILD = "$Z_BASE/child"
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvVar{
		{Name: "Z_BASE", Value: "base"},
		{Name: "A_CHILD", Value: "$Z_BASE/child"},
	}
	if len(recipe.Env) != len(want) {
		t.Fatalf("env = %#v, want %#v", recipe.Env, want)
	}
	for i := range want {
		if recipe.Env[i] != want[i] {
			t.Fatalf("env[%d] = %#v, want %#v", i, recipe.Env[i], want[i])
		}
	}
}

func TestTypedKeyringEntriesRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prod.keyring.pekit.toml")
	writeFile(t, path, `
[tcb]
priv = { path = "/run/key" }
`)
	if _, err := loadKeyring(path); err == nil {
		t.Fatal("expected typed keyring entry to fail")
	}
}

func TestPackageRefsNormalizeByOwner(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "packages.pekit", "pkg.package.pekit.toml"), `
[files]
"bin/tool" = "usr/bin/tool"
`)
	layers, err := LoadPackageLayers(dir, "source")
	if err != nil {
		t.Fatalf("load layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("expected one layer, got %d", len(layers))
	}
	if _, ok := layers[0].Config.Files["@source:bin/tool"]; !ok {
		t.Fatalf("source-owned file ref was not normalized: %#v", layers[0].Config.Files)
	}
}

func TestDuplicatePackageBaseFilesError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `format = "tar"`)
	writeFile(t, filepath.Join(dir, "packages.pekit", "package.pekit.toml"), `format = "tar"`)
	if _, err := LoadPackageLayers(dir, "recipe"); err == nil {
		t.Fatal("expected duplicate package base files to fail")
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPackageLicenseClass(t *testing.T) {
	load := func(class string) (PackageConfig, error) {
		path := filepath.Join(t.TempDir(), "package.pekit.toml")
		if err := os.WriteFile(path, []byte("[package]\nlicense_class = \""+class+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadPackageFile(path)
	}
	for _, want := range []string{"unknown", "free", "firmware", "proprietary"} {
		cfg, err := load(want)
		if err != nil {
			t.Fatalf("license_class %q: %v", want, err)
		}
		if cfg.Package.LicenseClass != want {
			t.Errorf("license_class %q: got %q", want, cfg.Package.LicenseClass)
		}
	}
	if _, err := load("nonfree"); err == nil {
		t.Fatal("license_class = nonfree accepted")
	}
}

// PEI-489: a test stage runs in a composed root like a build does, so it
// must be able to declare what that root needs. install/clean still cannot.
func TestTestTargetsAcceptDependencies(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = "make"

[test.smoke]
command = "./smoke.sh"

[test.smoke.dependencies.peipkg]
dash = "*"
bash = "*"
`)
	recipe, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err != nil {
		t.Fatalf("load recipe: %v", err)
	}
	if recipe.Targets[CommandTest]["smoke"].Dependencies["peipkg"]["dash"] != "*" {
		t.Fatalf("test target dependencies not decoded: %#v", recipe.Targets[CommandTest]["smoke"])
	}
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = "make"

[install.main]
command = "make install"

[install.main.dependencies.peipkg]
dash = "*"
`)
	if _, err := LoadRecipe(filepath.Join(dir, "pekit.toml")); err == nil {
		t.Fatal("install target accepted dependencies")
	}
}
