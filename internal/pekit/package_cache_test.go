package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A two-member recipe whose build target records every run, so a test can
// tell a reused build stage from a recompiled one.
const cacheTestRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build.main]
clear_out = false
command = 'printf x >> "$PEKIT_OUT/runs"'
`

const cacheTestMemberA = `
format = "peipkg"
builds = ["main"]

[package]
name = "app"
version = "{{version}}-1"
architecture = "x86_64"
description = "app"
license = "MIT"

[files]
"@source:payload.txt" = "usr/share/app/payload.txt"
`

const cacheTestMemberB = `
format = "peipkg"
builds = ["main"]

[package]
name = "app-extra"
version = "{{version}}-1"
architecture = "x86_64"
description = "app extra"
license = "MIT"

[files]
"@source:payload.txt" = "usr/share/app-extra/payload.txt"
`

// newPackageCacheRecipe writes the two-member recipe into a fresh git work
// tree and returns its root plus an App to drive it.
func newPackageCacheRecipe(t *testing.T, extra map[string]string) (string, *App, *bytes.Buffer) {
	t.Helper()
	recipe := filepath.Join(t.TempDir(), "app")
	serveURLs(t, map[string][]byte{
		"https://example.test/app-1.0.tar.gz": makeTarGz(t, "app-1.0", "payload\n"),
	})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), cacheTestRecipe)
	writeFile(t, filepath.Join(recipe, "a.package.pekit.toml"), cacheTestMemberA)
	writeFile(t, filepath.Join(recipe, "b.package.pekit.toml"), cacheTestMemberB)
	writeFile(t, filepath.Join(recipe, ".gitignore"), "out/\ndist/\n")
	for name, body := range extra {
		writeFile(t, filepath.Join(recipe, name), body)
	}
	runTestCmd(t, recipe, "git", "init")
	runTestCmd(t, recipe, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, recipe, "git", "config", "user.name", "Test")
	runTestCmd(t, recipe, "git", "add", ".")
	runTestCmd(t, recipe, "git", "commit", "-m", "recipe")
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	return recipe, &App{Stdout: &stdout, Stderr: &stderr}, &stderr
}

// stagedRecipeRefs maps every artifact left in the package stages to the
// recipe_ref its manifest carries. A packaging run's out tree is collected
// by artifact name, so an artifact of any package counts, selected or not.
func stagedRecipeRefs(t *testing.T, recipe string) map[string]string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(recipe, "out/*/package/*/*.peipkg"))
	if err != nil {
		t.Fatalf("glob package stages: %v", err)
	}
	refs := map[string]string{}
	for _, path := range matches {
		refs[filepath.Base(path)] = readPeipkgManifest(t, path).Build.RecipeRef
	}
	return refs
}

func assertNoStaleRefs(t *testing.T, refs map[string]string, want string) {
	t.Helper()
	if len(refs) == 0 {
		t.Fatal("no staged artifacts found")
	}
	for name, ref := range refs {
		if ref != want {
			t.Errorf("staged artifact %s carries recipe_ref %q, want %q", name, ref, want)
		}
	}
}

func buildRuns(t *testing.T, recipe string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(recipe, "out/*/build/main/runs"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("build run counter not found (%v): %v", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read build run counter: %v", err)
	}
	return len(data)
}

// TestPackageStagesDropStaleProvenanceOnCommit covers the dirty-to-clean
// transition: the first run packs both members from a dirty tree, the
// recipe is then committed, and a run that selects only one member must
// not leave the other member's `+dirty` artifact staged. The build stage
// is reused throughout, so invalidating the package cache costs no
// recompilation.
func TestPackageStagesDropStaleProvenanceOnCommit(t *testing.T) {
	recipe, app, stderr := newPackageCacheRecipe(t, nil)
	if err := app.Run([]string{"package", "--all", "--version", "1.0"}); err != nil {
		t.Fatalf("package --all failed: %v\nstderr=%s", err, stderr.String())
	}
	dirtyRef := "git:" + gitHead(t, recipe) + "+dirty"
	assertNoStaleRefs(t, stagedRecipeRefs(t, recipe), dirtyRef)
	if runs := buildRuns(t, recipe); runs != 1 {
		t.Fatalf("build target ran %d times, want 1", runs)
	}

	runTestCmd(t, recipe, "git", "add", "-A")
	runTestCmd(t, recipe, "git", "commit", "-m", "pin source")
	cleanRef := "git:" + gitHead(t, recipe)

	if err := app.Run([]string{"package", "a", "--version", "1.0", "--no-build"}); err != nil {
		t.Fatalf("package a failed: %v\nstderr=%s", err, stderr.String())
	}
	refs := stagedRecipeRefs(t, recipe)
	if _, ok := refs["app-extra_1.0-1_x86_64.peipkg"]; ok {
		t.Errorf("unselected member's stage survived a recipe commit: %v", refs)
	}
	assertNoStaleRefs(t, refs, cleanRef)
	if runs := buildRuns(t, recipe); runs != 1 {
		t.Errorf("build target ran %d times, want 1: dropping a package stage forced a recompilation", runs)
	}
}

// TestPackageStagesDropStaleProvenanceWhenTreeGoesDirty covers the
// clean-to-dirty transition in the same shape.
func TestPackageStagesDropStaleProvenanceWhenTreeGoesDirty(t *testing.T) {
	recipe, app, stderr := newPackageCacheRecipe(t, nil)
	if err := app.Run([]string{"package", "--all", "--version", "1.0"}); err != nil {
		t.Fatalf("package --all failed: %v\nstderr=%s", err, stderr.String())
	}
	runTestCmd(t, recipe, "git", "add", "-A")
	runTestCmd(t, recipe, "git", "commit", "-m", "pin source")
	if err := app.Run([]string{"package", "--all", "--version", "1.0", "--no-build"}); err != nil {
		t.Fatalf("clean package --all failed: %v\nstderr=%s", err, stderr.String())
	}
	cleanRef := "git:" + gitHead(t, recipe)
	assertNoStaleRefs(t, stagedRecipeRefs(t, recipe), cleanRef)

	writeFile(t, filepath.Join(recipe, "a.package.pekit.toml"), cacheTestMemberA+"\n# local edit\n")
	if err := app.Run([]string{"package", "a", "--version", "1.0", "--no-build"}); err != nil {
		t.Fatalf("dirty package a failed: %v\nstderr=%s", err, stderr.String())
	}
	refs := stagedRecipeRefs(t, recipe)
	if _, ok := refs["app-extra_1.0-1_x86_64.peipkg"]; ok {
		t.Errorf("unselected member's clean-tree stage survived the tree going dirty: %v", refs)
	}
	assertNoStaleRefs(t, refs, cleanRef+"+dirty")
}

// TestPackageStagesDropStaleProvenanceOnSecondCommit covers a commit-to-
// commit change, where both refs are clean and only the commit moves.
func TestPackageStagesDropStaleProvenanceOnSecondCommit(t *testing.T) {
	recipe, app, stderr := newPackageCacheRecipe(t, nil)
	if err := app.Run([]string{"package", "--all", "--version", "1.0"}); err != nil {
		t.Fatalf("package --all failed: %v\nstderr=%s", err, stderr.String())
	}
	runTestCmd(t, recipe, "git", "add", "-A")
	runTestCmd(t, recipe, "git", "commit", "-m", "pin source")
	if err := app.Run([]string{"package", "--all", "--version", "1.0", "--no-build"}); err != nil {
		t.Fatalf("first clean package failed: %v\nstderr=%s", err, stderr.String())
	}
	first := "git:" + gitHead(t, recipe)
	assertNoStaleRefs(t, stagedRecipeRefs(t, recipe), first)

	writeFile(t, filepath.Join(recipe, "NOTES"), "unrelated\n")
	runTestCmd(t, recipe, "git", "add", "-A")
	runTestCmd(t, recipe, "git", "commit", "-m", "notes")
	second := "git:" + gitHead(t, recipe)
	if first == second {
		t.Fatal("second commit did not move HEAD")
	}
	if err := app.Run([]string{"package", "b", "--version", "1.0", "--no-build"}); err != nil {
		t.Fatalf("package b failed: %v\nstderr=%s", err, stderr.String())
	}
	refs := stagedRecipeRefs(t, recipe)
	if _, ok := refs["app_1.0-1_x86_64.peipkg"]; ok {
		t.Errorf("unselected member's stage survived a new commit: %v", refs)
	}
	assertNoStaleRefs(t, refs, second)
}

// TestPackageStagesSurviveUnchangedProvenance is the other half of the
// gate: packaging one member must not throw away another member's staged
// artifact when nothing about the run's provenance moved. Otherwise
// `pekit package a && pekit package b` could never leave both artifacts
// staged for collection.
func TestPackageStagesSurviveUnchangedProvenance(t *testing.T) {
	recipe, app, stderr := newPackageCacheRecipe(t, nil)
	if err := app.Run([]string{"package", "a", "--version", "1.0"}); err != nil {
		t.Fatalf("package a failed: %v\nstderr=%s", err, stderr.String())
	}
	if err := app.Run([]string{"package", "b", "--version", "1.0", "--no-build"}); err != nil {
		t.Fatalf("package b failed: %v\nstderr=%s", err, stderr.String())
	}
	refs := stagedRecipeRefs(t, recipe)
	for _, want := range []string{"app_1.0-1_x86_64.peipkg", "app-extra_1.0-1_x86_64.peipkg"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("%s is not staged after a run with unchanged provenance: %v", want, refs)
		}
	}
	assertNoStaleRefs(t, refs, "git:"+gitHead(t, recipe)+"+dirty")
}

// TestPublishDropsStalePackageStages covers the same gate on the publish
// path, where a stale staged artifact is one `pekit publish` away from a
// signed repository.
func TestPublishDropsStalePackageStages(t *testing.T) {
	publishTarget := "\n[[publish.localdir]]\npath = \"dist\"\n"
	recipe, app, stderr := newPackageCacheRecipe(t, map[string]string{
		"a.package.pekit.toml": cacheTestMemberA + publishTarget,
		"b.package.pekit.toml": cacheTestMemberB + publishTarget,
	})
	if err := app.Run([]string{"publish", "--all", "--version", "1.0", "--allow-unsigned"}); err != nil {
		t.Fatalf("publish --all failed: %v\nstderr=%s", err, stderr.String())
	}
	dirtyRef := "git:" + gitHead(t, recipe) + "+dirty"
	assertNoStaleRefs(t, stagedRecipeRefs(t, recipe), dirtyRef)

	runTestCmd(t, recipe, "git", "add", "-A")
	runTestCmd(t, recipe, "git", "commit", "-m", "pin source")
	cleanRef := "git:" + gitHead(t, recipe)
	if err := app.Run([]string{"publish", "a", "--version", "1.0", "--no-build", "--allow-unsigned"}); err != nil {
		t.Fatalf("publish a failed: %v\nstderr=%s", err, stderr.String())
	}
	refs := stagedRecipeRefs(t, recipe)
	if _, ok := refs["app-extra_1.0-1_x86_64.peipkg"]; ok {
		t.Errorf("unselected member's stage survived a recipe commit: %v", refs)
	}
	assertNoStaleRefs(t, refs, cleanRef)
	published := filepath.Join(recipe, "dist", "app_1.0-1_x86_64.peipkg")
	if ref := readPeipkgManifest(t, published).Build.RecipeRef; ref != cleanRef {
		t.Errorf("published artifact carries recipe_ref %q, want %q", ref, cleanRef)
	}
}

// TestPackageStageStampRecordsProvenance pins the stamp itself: it names
// the run provenance and the package identity the stage holds, and lives
// beside the stage rather than inside it, where it would become payload.
func TestPackageStageStampRecordsProvenance(t *testing.T) {
	recipe, app, stderr := newPackageCacheRecipe(t, nil)
	if err := app.Run([]string{"package", "a", "--version", "1.0"}); err != nil {
		t.Fatalf("package a failed: %v\nstderr=%s", err, stderr.String())
	}
	stage := filepath.Dir(globOne(t, filepath.Join(recipe, "out/*/package/*/app_1.0-1_x86_64.peipkg")))
	stamps, err := filepath.Glob(filepath.Join(recipe, "out/*/.pekit/packages/"+filepath.Base(stage)+".json"))
	if err != nil || len(stamps) != 1 {
		t.Fatalf("stamp for stage %s not found: %v %v", filepath.Base(stage), stamps, err)
	}
	data, err := os.ReadFile(stamps[0])
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	for _, want := range []string{
		`"recipe_ref": "git:` + gitHead(t, recipe) + `+dirty"`,
		`"name": "app"`,
		`"version": "1.0-1"`,
		`"format": "peipkg"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("stamp %s does not record %s", data, want)
		}
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatalf("read stage: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			t.Errorf("stage holds metadata %q that would become package payload", entry.Name())
		}
	}
}
