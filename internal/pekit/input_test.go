package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A recipe with one source and one input: the build reads both, so a pass
// proves the input was fetched, materialised and exported to the target.
const inputTestRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[input.kernel]
url = "https://example.test/linux-7.0.9.tar.gz"
versions = "= 7.0.9"
extract = true
root = "linux-7.0.9"

[build]
command = '''
set -eu
cat payload.txt > "$PEKIT_OUT/source"
cat "$PEKIT_INPUT_KERNEL/payload.txt" > "$PEKIT_OUT/input"
'''
`

func TestInputIsFetchedLockedAndExportedToTargets(t *testing.T) {
	dir := t.TempDir()
	sourceURL := "https://example.test/app-1.0.tar.gz"
	inputURL := "https://example.test/linux-7.0.9.tar.gz"
	responses := map[string][]byte{
		sourceURL: makeTarGz(t, "app-1.0", "from-source"),
		inputURL:  makeTarGz(t, "linux-7.0.9", "from-input"),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), inputTestRecipe)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}

	// The input's payload reached the target, so PEKIT_INPUT_KERNEL pointed at
	// the extracted tree with its root directory stripped.
	stage := findStageFile(t, dir, "input")
	if got := strings.TrimSpace(stage); got != "from-input" {
		t.Fatalf("input payload = %q, want %q", got, "from-input")
	}

	lock, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.FindInput("kernel")
	if entry == nil {
		t.Fatal("expected a lock entry for input kernel")
	}
	if want := sha256Hex(responses[inputURL]); entry.SHA256 != want {
		t.Fatalf("locked sha256 = %s, want %s", entry.SHA256, want)
	}
	if entry.Version != "7.0.9" {
		t.Fatalf("locked version = %s, want 7.0.9", entry.Version)
	}
	if entry.URL != inputURL {
		t.Fatalf("locked url = %s, want %s", entry.URL, inputURL)
	}
	// The recipe's own source pins independently of the input.
	if lock.Find("1.0") == nil {
		t.Fatal("expected the source to stay locked alongside the input")
	}
}

func TestInputRefetchDetectsChangedUpstream(t *testing.T) {
	dir := t.TempDir()
	sourceURL := "https://example.test/app-1.0.tar.gz"
	inputURL := "https://example.test/linux-7.0.9.tar.gz"
	responses := map[string][]byte{
		sourceURL: makeTarGz(t, "app-1.0", "from-source"),
		inputURL:  makeTarGz(t, "linux-7.0.9", "from-input"),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), inputTestRecipe)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("first build failed: %v\nstderr=%s", err, stderr.String())
	}

	// Upstream republishes different bytes under the same pinned version. The
	// cache still matches the lock, so a plain rebuild passes; a re-download
	// must stop rather than silently building something else.
	responses[inputURL] = makeTarGz(t, "linux-7.0.9", "tampered")
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("cached rebuild failed: %v\nstderr=%s", err, stderr.String())
	}
	err := app.Run([]string{"build", "--version", "1.0", "--refresh-source"})
	if err == nil {
		t.Fatal("expected a lock mismatch after the input's upstream changed")
	}
	if diagCode(err) != "lock_mismatch" {
		t.Fatalf("expected lock_mismatch, got %v", err)
	}
}

func TestInputRejectsRangeConstraint(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[input.kernel]
url = "https://example.test/linux-{{version}}.tar.gz"
versions = ">= 7.0.9"

[build]
command = "true"
`)
	_, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err == nil {
		t.Fatal("expected a range constraint on an input to be rejected")
	}
	if diagCode(err) != "invalid_versions" {
		t.Fatalf("expected invalid_versions, got %v (%s)", diagCode(err), err)
	}
	if !strings.Contains(err.Error(), "pin one version") {
		t.Fatalf("diagnostic should say an input pins one version, got: %s", err)
	}
}

func TestInputNameCollisionInEnvIsRejected(t *testing.T) {
	dir := t.TempDir()
	// "a-b" and "a" + "b" cannot collide, but "a-b" and "a_b" would — names are
	// restricted so the PEKIT_INPUT_ mapping stays injective.
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[input.Kernel]
url = "https://example.test/linux-7.0.9.tar.gz"
versions = "= 7.0.9"

[build]
command = "true"
`)
	_, err := LoadRecipe(filepath.Join(dir, "pekit.toml"))
	if err == nil {
		t.Fatal("expected an upper-case input name to be rejected")
	}
	if diagCode(err) != "invalid_name" {
		t.Fatalf("expected invalid_name, got %v (%s)", diagCode(err), err)
	}
}

// findStageFile returns the contents of a file written into a build stage,
// wherever the scope hash placed it.
func findStageFile(t *testing.T, recipeRoot, name string) string {
	t.Helper()
	var found string
	err := filepath.Walk(filepath.Join(recipeRoot, "out"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != name {
			return nil //nolint:nilerr // a missing branch is not a failure here
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		found = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walking build output: %v", err)
	}
	if found == "" {
		t.Fatalf("no stage file named %q under %s", name, filepath.Join(recipeRoot, "out"))
	}
	return found
}
