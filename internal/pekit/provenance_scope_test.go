package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A workspace of two independent package recipes sharing one source, one
// set of inherited defaults, and one publish conduit — the shape of pkgs.
const scopeWorkspace = `
include = ["./*"]
`

const scopeSharedPackageDefaults = `
format = "peipkg"

[package]
architecture = "x86_64"
license = "MIT"
`

const scopeMemberRecipe = `
out_dir = "out"

[source]
patches = "patches"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build.main]
command = "true"
`

func scopeMemberPackage(name string) string {
	return `
[package]
name = "` + name + `"
version = "{{version}}-1"
description = "` + name + `"

[files]
"@source:payload.txt" = "usr/share/` + name + `/payload.txt"

[[publish.localdir]]
path = "_pool_"
`
}

// newScopeWorkspace builds the workspace in a git work tree and commits
// it whole, so every later state is a deliberate change. `ignore` seeds
// the .gitignore, which the output-directory tests deliberately leave
// empty: provenance must not depend on a recipe repository having been
// taught to ignore pekit's own managed output.
func newScopeWorkspace(t *testing.T, ignore string) (string, *App, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	serveURLs(t, map[string][]byte{
		"https://example.test/app-1.0.tar.gz": makeTarGz(t, "app-1.0", "payload\n"),
	})
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), scopeWorkspace)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), scopeSharedPackageDefaults)
	writeFile(t, filepath.Join(dir, ".gitignore"), ignore)
	for _, name := range []string{"alpha", "beta"} {
		writeFile(t, filepath.Join(dir, name, "pekit.toml"), scopeMemberRecipe)
		writeFile(t, filepath.Join(dir, name, "package.pekit.toml"), scopeMemberPackage(name))
		writeFile(t, filepath.Join(dir, name, "patches", "series"), "")
	}
	runTestCmd(t, dir, "git", "init")
	runTestCmd(t, dir, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, dir, "git", "config", "user.name", "Test")
	runTestCmd(t, dir, "git", "add", ".")
	gitCommitFixed(t, dir, "workspace")
	var stdout, stderr bytes.Buffer
	return dir, &App{Stdout: &stdout, Stderr: &stderr}, &stderr
}

// packageAlphaRef packages the alpha member and returns the recipe_ref
// its manifest recorded. The lock the run writes is committed first, so
// the only uncommitted state is whatever the test set up.
func packageAlphaRef(t *testing.T, dir string, app *App, stderr *bytes.Buffer, args ...string) string {
	t.Helper()
	alpha := filepath.Join(dir, "alpha")
	chdir(t, alpha)
	if err := app.Run(append([]string{"package", "--version", "1.0"}, args...)); err != nil {
		t.Fatalf("package alpha failed: %v\nstderr=%s", err, stderr.String())
	}
	artifact := globOne(t, filepath.Join(alpha, "out/*/package/*/alpha_1.0-1_x86_64.peipkg"))
	return readPeipkgManifest(t, artifact).Build.RecipeRef
}

// commitLock commits the lockfile a first run writes, so later runs
// measure only the state the test arranges.
func commitLock(t *testing.T, dir string) {
	t.Helper()
	runTestCmd(t, dir, "git", "add", "-A", "--", "alpha/pekit.lock")
	gitCommitFixed(t, dir, "pin sources")
}

// TestDirtyIgnoresOtherWorkspaceMembers is the headline case: editing one
// package recipe must not taint the provenance of another.
func TestDirtyIgnoresOtherWorkspaceMembers(t *testing.T) {
	dir, app, stderr := newScopeWorkspace(t, "out\n_pool_\n")
	packageAlphaRef(t, dir, app, stderr)
	commitLock(t, dir)

	writeFile(t, filepath.Join(dir, "beta", "pekit.toml"), scopeMemberRecipe+"\n# unrelated work\n")
	writeFile(t, filepath.Join(dir, "beta", "NEW-FILE"), "untracked\n")
	ref := packageAlphaRef(t, dir, app, stderr, "--no-build")
	if want := "git:" + gitHead(t, dir); ref != want {
		t.Errorf("recipe_ref = %q, want %q: an edit to another member tainted alpha", ref, want)
	}
}

// TestDirtyIgnoresManagedOutputDirectories covers a real out_dir and a
// publish conduit that the repository has not been taught to ignore. Both
// are pekit's own managed output, so neither describes the recipe.
func TestDirtyIgnoresManagedOutputDirectories(t *testing.T) {
	dir, app, stderr := newScopeWorkspace(t, "")
	packageAlphaRef(t, dir, app, stderr)
	commitLock(t, dir)

	// An operational publish conduit, already holding artifacts.
	writeFile(t, filepath.Join(dir, "_pool_", "alpha_0.9-1_x86_64.peipkg"), "earlier release\n")
	ref := packageAlphaRef(t, dir, app, stderr, "--no-build")
	if want := "git:" + gitHead(t, dir); ref != want {
		t.Errorf("recipe_ref = %q, want %q: managed output tainted the recipe", ref, want)
	}
}

// TestDirtyIgnoresLinkedOutputDirectory covers the release-worktree shape:
// a member's out_dir is a symlink into shared staging rather than a real
// directory, so git sees an untracked *file* beside the recipe.
func TestDirtyIgnoresLinkedOutputDirectory(t *testing.T) {
	dir, app, stderr := newScopeWorkspace(t, "")
	shared := filepath.Join(dir, "_shared_", "alpha")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(dir, "alpha", "out")); err != nil {
		t.Fatal(err)
	}
	// The shared staging tree itself is managed output too, and lives
	// outside every member, so it is ignored the way a conduit is.
	writeFile(t, filepath.Join(dir, ".gitignore"), "_shared_\n_pool_\n")
	runTestCmd(t, dir, "git", "add", "-A", "--", ".gitignore")
	gitCommitFixed(t, dir, "ignore shared staging")

	packageAlphaRef(t, dir, app, stderr)
	commitLock(t, dir)
	ref := packageAlphaRef(t, dir, app, stderr, "--no-build")
	if want := "git:" + gitHead(t, dir); ref != want {
		t.Errorf("recipe_ref = %q, want %q: a linked out_dir tainted the recipe", ref, want)
	}
	if staged, _ := filepath.Glob(filepath.Join(shared, "*", "package")); len(staged) == 0 {
		t.Errorf("the linked out_dir was not the one written to")
	}
}

// TestDirtyStillMarksEffectiveInputs is the other half: every input that
// can change what alpha packages still marks it dirty.
func TestDirtyStillMarksEffectiveInputs(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"the selected recipe", "alpha/pekit.toml", scopeMemberRecipe + "\n# local change\n"},
		{"the selected package definition", "alpha/package.pekit.toml", scopeMemberPackage("alpha") + "\n"},
		{"an inherited workspace input", "workspace.pekit.toml", scopeWorkspace + "\n"},
		{"inherited package defaults", "package.pekit.toml", scopeSharedPackageDefaults + "\n"},
		{"an inherited environment file", "env.pekit.toml", "[env]\nEXTRA = \"1\"\n"},
		{"an embedded patch", "alpha/patches/series", "# none yet\n"},
		{"an embedded key", "alpha/keys/upstream.asc", "not a real key\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, app, stderr := newScopeWorkspace(t, "out\n_pool_\n")
			packageAlphaRef(t, dir, app, stderr)
			commitLock(t, dir)
			writeFile(t, filepath.Join(dir, filepath.FromSlash(tc.path)), tc.body)
			ref := packageAlphaRef(t, dir, app, stderr, "--no-build")
			if want := "git:" + gitHead(t, dir) + "+dirty"; ref != want {
				t.Errorf("recipe_ref = %q, want %q: changing %s did not mark the build dirty", ref, want, tc.path)
			}
		})
	}
}
