package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const patchTestDiff = `--- a/payload.txt
+++ b/payload.txt
@@ -1 +1 @@
-payload
+patched
`

const patchTestReworkedDiff = `--- a/payload.txt
+++ b/payload.txt
@@ -1 +1 @@
-payload
+reworked
`

const patchedURLRecipe = `
out_dir = "out"

[source]
patches = "patches"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build]
command = "cat payload.txt > \"$PEKIT_RECIPE_ROOT/result.txt\""
`

func readBuildValue(t *testing.T, recipe string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(recipe, "result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestURLSourcePatchesAppliedAndInvalidateOnEdit(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(dir, "pekit.toml"), patchedURLRecipe)
	writeFile(t, filepath.Join(dir, "patches", "series"), "# comment line\nfix.patch # inline comment\n")
	writeFile(t, filepath.Join(dir, "patches", "fix.patch"), patchTestDiff)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	if got := readBuildValue(t, dir); got != "patched\n" {
		t.Fatalf("built value = %q, want patched", got)
	}

	// Editing a patch must invalidate the materialised tree: the set hash
	// joins the url scope, so the next build extracts and patches afresh.
	writeFile(t, filepath.Join(dir, "patches", "fix.patch"), patchTestReworkedDiff)
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("rebuild after patch edit failed: %v\nstderr=%s", err, stderr.String())
	}
	if got := readBuildValue(t, dir); got != "reworked\n" {
		t.Fatalf("built value after edit = %q, want reworked", got)
	}
}

func TestGitSourcePatchesReappliedEachResolve(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "payload\n")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v1.0.0")

	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source]
patches = "patches"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = "cat payload.txt > \"$PEKIT_RECIPE_ROOT/result.txt\""
`)
	writeFile(t, filepath.Join(recipe, "patches", "series"), "fix.patch\n")
	writeFile(t, filepath.Join(recipe, "patches", "fix.patch"), patchTestDiff)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	for i := 0; i < 2; i++ {
		if err := app.Run([]string{"build", "--version", "1.0.0"}); err != nil {
			t.Fatalf("build %d failed: %v\nstderr=%s", i+1, err, stderr.String())
		}
		if got := readBuildValue(t, recipe); got != "patched\n" {
			t.Fatalf("build %d value = %q, want patched", i+1, got)
		}
	}
}

func TestPatchSeriesValidation(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(dir, "pekit.toml"), patchedURLRecipe)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}

	// A series entry with no file behind it.
	writeFile(t, filepath.Join(dir, "patches", "series"), "missing.patch\n")
	err := app.Run([]string{"build", "--version", "1.0"})
	if diagCode(err) != "patch_missing" {
		t.Fatalf("expected patch_missing, got %v", err)
	}

	// A patch file the series does not list, with and without --allow-unused.
	writeFile(t, filepath.Join(dir, "patches", "series"), "fix.patch\n")
	writeFile(t, filepath.Join(dir, "patches", "fix.patch"), patchTestDiff)
	writeFile(t, filepath.Join(dir, "patches", "stray.patch"), patchTestDiff)
	err = app.Run([]string{"build", "--version", "1.0"})
	if diagCode(err) != "unused_patch" {
		t.Fatalf("expected unused_patch, got %v", err)
	}
	if err := app.Run([]string{"build", "--version", "1.0", "--allow-unused"}); err != nil {
		t.Fatalf("--allow-unused build failed: %v\nstderr=%s", err, stderr.String())
	}
	if got := readBuildValue(t, dir); got != "patched\n" {
		t.Fatalf("built value = %q, want patched", got)
	}
}

func TestPatchTargetMissingFailsAndNothingIsCached(t *testing.T) {
	dir := t.TempDir()
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(dir, "pekit.toml"), patchedURLRecipe)
	writeFile(t, filepath.Join(dir, "patches", "series"), "fix.patch\n")
	writeFile(t, filepath.Join(dir, "patches", "fix.patch"), strings.ReplaceAll(patchTestDiff, "payload.txt", "absent.txt"))
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	for i := 0; i < 2; i++ {
		err := app.Run([]string{"build", "--version", "1.0"})
		if code := diagCode(err); code != "patch_apply" && code != "patch_skipped" {
			t.Fatalf("run %d: expected patch_apply or patch_skipped, got %v", i+1, err)
		}
	}
}

func TestSourcePackageCarriesRenamedPatchesDir(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), strings.Replace(patchedURLRecipe, `patches = "patches"`, `patches = "peios-patches"`, 1))
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), sourcePkgMemberDef)
	writeFile(t, filepath.Join(recipe, "peios-patches", "series"), "fix.patch\n")
	writeFile(t, filepath.Join(recipe, "peios-patches", "fix.patch"), patchTestDiff)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	srcArtifact := globOne(t, filepath.Join(recipe, "out", "*", "package", "*", "app-source_1.0-1_noarch.peipkg"))
	entries := readPeipkgEntries(t, srcArtifact)
	root := "usr/src/dist/app-1.0-1"
	for _, want := range []string{root + "/patches/series", root + "/patches/fix.patch"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("expected %s in source package; entries: %v", want, sortedKeys(entriesPresent(entries)))
		}
	}
}
