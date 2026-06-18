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
}

func TestLoadRecipeRejectsCamelCaseOutDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `outDir = "out"`)
	_, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err == nil {
		t.Fatal("expected outDir to be rejected in v2")
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
