package pekit

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestManifestCarriesRecipeProvenance covers the recipe_ref/builder
// build-block fields end to end: a dirty recipe tree (the freshly
// written pekit.lock is untracked) is marked +dirty; committing the
// lock makes the next run pin the clean commit; both runs identify the
// producing pekit.
func TestManifestCarriesRecipeProvenance(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), signTestRecipe)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), signTestMemberDef)
	writeFile(t, filepath.Join(recipe, ".gitignore"), "out/\n")
	runTestCmd(t, recipe, "git", "init")
	runTestCmd(t, recipe, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, recipe, "git", "config", "user.name", "Test")
	runTestCmd(t, recipe, "git", "add", ".")
	runTestCmd(t, recipe, "git", "commit", "-m", "recipe")
	chdir(t, recipe)

	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	artifact := globOne(t, filepath.Join(recipe, "out/*/package/*/app_1.0-1_x86_64.peipkg"))
	m := readPeipkgManifest(t, artifact)
	// The run just wrote pekit.lock, which is untracked: the tree is
	// dirty, and the manifest must say so.
	if want := "git:" + gitHead(t, recipe) + "+dirty"; m.Build.RecipeRef != want {
		t.Errorf("dirty-tree recipe_ref = %q, want %q", m.Build.RecipeRef, want)
	}
	if !strings.HasPrefix(m.Build.Builder, "pekit/") || m.Build.Builder == "pekit/" {
		t.Errorf("builder = %q, want a pekit/<id> identity", m.Build.Builder)
	}

	// Committing the lock cleans the tree; the next run pins the commit.
	runTestCmd(t, recipe, "git", "add", "pekit.lock")
	runTestCmd(t, recipe, "git", "commit", "-m", "pin source")
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("second package failed: %v\nstderr=%s", err, stderr.String())
	}
	m = readPeipkgManifest(t, artifact)
	if want := "git:" + gitHead(t, recipe); m.Build.RecipeRef != want {
		t.Errorf("clean-tree recipe_ref = %q, want %q", m.Build.RecipeRef, want)
	}

	// The corresponding-source package carries the same provenance.
	src := globOne(t, filepath.Join(recipe, "out/*/package/*/app-source_1.0-1_noarch.peipkg"))
	sm := readPeipkgManifest(t, src)
	if sm.Build.RecipeRef != m.Build.RecipeRef || sm.Build.Builder != m.Build.Builder {
		t.Errorf("source package provenance %q/%q differs from member %q/%q",
			sm.Build.RecipeRef, sm.Build.Builder, m.Build.RecipeRef, m.Build.Builder)
	}
}

func TestRecipeRefAbsentOutsideGit(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "app")
	rawURL := "https://example.test/app-1.0.tar.gz"
	serveURLs(t, map[string][]byte{rawURL: makeTarGz(t, "app-1.0", "payload\n")})
	writeFile(t, filepath.Join(recipe, "pekit.toml"), signTestRecipe)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), signTestMemberDef)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--version", "1.0"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}
	artifact := globOne(t, filepath.Join(recipe, "out/*/package/*/app_1.0-1_x86_64.peipkg"))
	if m := readPeipkgManifest(t, artifact); m.Build.RecipeRef != "" {
		t.Errorf("recipe_ref outside a git tree = %q, want empty", m.Build.RecipeRef)
	}
}
