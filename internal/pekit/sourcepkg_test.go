package pekit

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const sourcePkgMemberDef = `
format = "peipkg"

[package]
name = "app"
version = "{{version}}-1"
architecture = "x86_64"
description = "app"
license = "MIT"

[files]
"@source:payload.txt" = "usr/share/app/payload.txt"
`

// readPeipkgEntries returns every entry in a .peipkg keyed by archive
// path, with file contents for regular files.
func readPeipkgEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[strings.TrimSuffix(hdr.Name, "/")] = data
	}
	return entries
}

func globOne(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one match for %s, got %v", pattern, matches)
	}
	return matches[0]
}

func TestURLSourceEmitsSourcePackage(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	upstream := makeTarGz(t, "app-1.0", "payload")
	serveURLs(t, map[string][]byte{rawURL: upstream})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), lockTestRecipe)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), sourcePkgMemberDef)
	writeFile(t, filepath.Join(recipe, "keys", "upstream.asc"), "not a real key")
	writeFile(t, filepath.Join(recipe, "dev.keyring.pekit.toml"), "[secrets]\ntoken = \"x\"\n")
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}

	binary := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app_1.0-1_x86_64.peipkg"))
	manifest := readPeipkgManifest(t, binary)
	if manifest.Build.SourcePackage != "app-source" {
		t.Fatalf("binary build.source_package = %q, want app-source", manifest.Build.SourcePackage)
	}

	srcArtifact := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app-source_1.0-1_noarch.peipkg"))
	srcManifest := readPeipkgManifest(t, srcArtifact)
	if srcManifest.License != "MIT" {
		t.Fatalf("source package license = %q, want MIT", srcManifest.License)
	}
	if srcManifest.Description != "Corresponding source for app 1.0-1" {
		t.Fatalf("source package description = %q", srcManifest.Description)
	}
	if srcManifest.Build.SourcePackage != "" {
		t.Fatalf("source package must not self-reference, got %q", srcManifest.Build.SourcePackage)
	}
	if !strings.HasPrefix(srcManifest.Build.SourceRef, "url:"+rawURL+"#sha256:") {
		t.Fatalf("source package source_ref = %q, want locked url provenance", srcManifest.Build.SourceRef)
	}

	entries := readPeipkgEntries(t, srcArtifact)
	root := "usr/src/dist/app-1.0-1"
	got, ok := entries[root+"/upstream/app-1.0.tar.gz"]
	if !ok {
		t.Fatalf("upstream artifact missing; entries: %v", sortedKeys(entriesPresent(entries)))
	}
	if !bytes.Equal(got, upstream) {
		t.Fatal("upstream artifact bytes differ from the pristine download")
	}
	for _, want := range []string{
		root + "/recipe/pekit.toml",
		root + "/recipe/package.pekit.toml",
		root + "/recipe/pekit.lock",
		root + "/recipe/keys/upstream.asc",
	} {
		if _, ok := entries[want]; !ok {
			t.Errorf("expected %s in source package; entries: %v", want, sortedKeys(entriesPresent(entries)))
		}
	}
	for name := range entries {
		if strings.Contains(name, "keyring") {
			t.Fatalf("dev keyring material leaked into source package: %s", name)
		}
	}
}

func entriesPresent(entries map[string][]byte) map[string]bool {
	out := map[string]bool{}
	for name := range entries {
		out[name] = true
	}
	return out
}

func TestGitSourceEmitsSourcePackageArchive(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v1.0.0")

	recipe := filepath.Join(dir, "app")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = "true"
`)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), sourcePkgMemberDef)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}

	srcArtifact := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app-source_1.0.0-1_noarch.peipkg"))
	entries := readPeipkgEntries(t, srcArtifact)
	archive, ok := entries["usr/src/dist/app-1.0.0-1/upstream/app-1.0.0-1.tar"]
	if !ok {
		t.Fatalf("git archive missing; entries: %v", sortedKeys(entriesPresent(entries)))
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "app-1.0.0-1/payload.txt" {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "payload" {
				t.Fatalf("archived payload = %q", data)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected app-1.0.0-1/payload.txt inside the git archive export")
	}
}

func TestSourcePackageDisabled(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), lockTestRecipe+`
[source_package]
enabled = false
`)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), sourcePkgMemberDef)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "*", "package", "*", "*-source_*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no source package, got %v", matches)
	}
	binary := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app_1.0-1_x86_64.peipkg"))
	if got := readPeipkgManifest(t, binary).Build.SourcePackage; got != "" {
		t.Fatalf("binary build.source_package = %q, want empty when disabled", got)
	}
}

func TestSourcePackageNameOverride(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app-lib")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), lockTestRecipe+`
[source_package]
name = "custom-source"
`)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), sourcePkgMemberDef)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	srcArtifact := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "custom-source_1.0-1_noarch.peipkg"))
	entries := readPeipkgEntries(t, srcArtifact)
	if _, ok := entries["usr/src/dist/custom-1.0-1/recipe/pekit.toml"]; !ok {
		t.Fatalf("expected payload root to follow the name override; entries: %v", sortedKeys(entriesPresent(entries)))
	}
	binary := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app_1.0-1_x86_64.peipkg"))
	if got := readPeipkgManifest(t, binary).Build.SourcePackage; got != "custom-source" {
		t.Fatalf("binary build.source_package = %q, want custom-source", got)
	}
}

func TestSourcePackageVersionConflict(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), lockTestRecipe)
	memberDef := func(name, version string) string {
		return `
format = "peipkg"

[package]
name = "` + name + `"
version = "` + version + `"
architecture = "x86_64"
description = "app"
license = "MIT"

[files]
"@source:payload.txt" = "usr/share/` + name + `/payload.txt"
`
	}
	writeFile(t, filepath.Join(recipe, "a.package.pekit.toml"), memberDef("app-a", "{{version}}-1"))
	writeFile(t, filepath.Join(recipe, "b.package.pekit.toml"), memberDef("app-b", "{{version}}-2"))
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"package", "--all", "--version", "1.0"})
	if err == nil {
		t.Fatal("expected version-conflict failure")
	}
	if diagCode(err) != "source_package_version_conflict" {
		t.Fatalf("expected source_package_version_conflict, got %v", err)
	}
}

func TestSourcelessRecipeEmitsNoSourcePackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"
`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
name = "app"
version = "1.0.0-1"
architecture = "x86_64"
description = "app"
license = "MIT"

[files]
"@recipe:payload.txt" = "usr/share/app/payload.txt"
`)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(dir, "out", "package", "*", "*-source_*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no source package for a sourceless recipe, got %v", matches)
	}
	binary := globOne(t, filepath.Join(dir, "out", "package", "*", "app_1.0.0-1_x86_64.peipkg"))
	if got := readPeipkgManifest(t, binary).Build.SourcePackage; got != "" {
		t.Fatalf("binary build.source_package = %q, want empty", got)
	}
}
