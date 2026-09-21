package pekit

import (
	"bytes"
	"encoding/hex"
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
	entry := lock.FindInput("kernel", "7.0.9")
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

// A delegated source declares the input its borrowed build reads. The
// delegating recipe declares nothing, yet the input is fetched, verified with
// the key committed beside the source's pekit.toml, and pinned in the
// delegating recipe's own lock — the source tree is never written to.
func TestDelegatedSourceDeclaresItsInput(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	inputURL := "https://example.test/linux-7.0.9.tar.gz"
	artifact := makeTarGz(t, "linux-7.0.9", "from-delegated-input")
	serveURLs(t, map[string][]byte{
		inputURL:          artifact,
		inputURL + ".sig": detachSign(t, signer, artifact),
	})
	source := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(source, "pekit.toml"), `
[input.kernel]
url = "https://example.test/linux-{{version}}.tar.gz"
versions = "= 7.0.9"
extract = true
root = "linux-{{version}}"

[input.kernel.signature]
key_files = ["keys/upstream.key"]

[build]
command = 'cat "$PEKIT_INPUT_KERNEL/payload.txt" > "$PEKIT_OUT/input"'
`)
	if err := os.MkdirAll(filepath.Join(source, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "keys", "upstream.key"), armoredPublicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"
delegate = true

[source.local]
path = "../src"
`)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--local"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	if got := strings.TrimSpace(findStageFile(t, recipe, "input")); got != "from-delegated-input" {
		t.Fatalf("input payload = %q, want %q", got, "from-delegated-input")
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.FindInput("kernel", "7.0.9")
	if entry == nil {
		t.Fatal("expected the delegating recipe to lock the source's input")
	}
	if want := hex.EncodeToString(signer.PrimaryKey.Fingerprint); entry.SignatureKey != want {
		t.Fatalf("signature_key = %q, want %q", entry.SignatureKey, want)
	}
	if fileExists(filepath.Join(source, "pekit.lock")) {
		t.Fatal("the delegated source tree must not be written to")
	}
}

// A recipe input replaces a delegated source's input of the same name as a
// whole, the same rule that governs delegated targets.
func TestRecipeInputReplacesDelegatedInput(t *testing.T) {
	dir := t.TempDir()
	serveURLs(t, map[string][]byte{
		"https://example.test/from-source-7.0.9.tar.gz": makeTarGz(t, "linux-7.0.9", "source-declared"),
		"https://example.test/from-recipe-7.0.9.tar.gz": makeTarGz(t, "linux-7.0.9", "recipe-declared"),
	})
	source := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(source, "pekit.toml"), `
[input.kernel]
url = "https://example.test/from-source-{{version}}.tar.gz"
versions = "= 7.0.9"
extract = true
root = "linux-{{version}}"

[build]
command = 'cat "$PEKIT_INPUT_KERNEL/payload.txt" > "$PEKIT_OUT/input"'
`)
	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"
delegate = true

[source.local]
path = "../src"

[input.kernel]
url = "https://example.test/from-recipe-{{version}}.tar.gz"
versions = "= 7.0.9"
extract = true
root = "linux-{{version}}"
`)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--local"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	if got := strings.TrimSpace(findStageFile(t, recipe, "input")); got != "recipe-declared" {
		t.Fatalf("input payload = %q, want the recipe's own input", got)
	}
}

// Without build delegation the source tree's pekit.toml is not the build's
// definition, so its inputs are not the build's either: nothing is fetched.
func TestUndelegatedRecipeIgnoresSourceInputs(t *testing.T) {
	dir := t.TempDir()
	serveURLs(t, map[string][]byte{})
	source := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(source, "pekit.toml"), `
[input.kernel]
url = "https://example.test/unserved-7.0.9.tar.gz"
versions = "= 7.0.9"

[build]
command = "false"
`)
	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.local]
path = "../src"

[build]
command = 'printf "%s" "${PEKIT_INPUT_KERNEL:-unset}" > "$PEKIT_OUT/input"'
`)
	chdir(t, recipe)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"build", "--local"}); err != nil {
		t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
	}
	if got := findStageFile(t, recipe, "input"); got != "unset" {
		t.Fatalf("PEKIT_INPUT_KERNEL = %q, want it unset", got)
	}
}

// Input pins are keyed by version, as [[source]] pins are: moving an input is
// an edit to its `versions`, the new version is verified and pinned on first
// use, and the old pin stays valid for builds that still name it. Each
// version is materialised apart, so a tree extracted for one is never served
// for another.
func TestInputPinsAreKeyedByVersion(t *testing.T) {
	dir := t.TempDir()
	responses := map[string][]byte{
		"https://example.test/app-1.0.tar.gz":      makeTarGz(t, "app-1.0", "from-source"),
		"https://example.test/linux-7.0.9.tar.gz":  makeTarGz(t, "linux-7.0.9", "seven-oh-nine"),
		"https://example.test/linux-7.0.10.tar.gz": makeTarGz(t, "linux-7.0.10", "seven-oh-ten"),
	}
	serveURLs(t, responses)
	recipeFor := func(version string) string {
		return strings.NewReplacer("linux-7.0.9", "linux-"+version, "= 7.0.9", "= "+version).Replace(inputTestRecipe)
	}
	writeFile(t, filepath.Join(dir, "pekit.toml"), recipeFor("7.0.9"))
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	build := func(want string) {
		t.Helper()
		if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
			t.Fatalf("build failed: %v\nstderr=%s", err, stderr.String())
		}
		if got := strings.TrimSpace(findStageFile(t, dir, "input")); got != want {
			t.Fatalf("input payload = %q, want %q", got, want)
		}
	}
	build("seven-oh-nine")
	writeFile(t, filepath.Join(dir, "pekit.toml"), recipeFor("7.0.10"))
	build("seven-oh-ten")
	writeFile(t, filepath.Join(dir, "pekit.toml"), recipeFor("7.0.9"))
	build("seven-oh-nine")

	lock, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"7.0.9", "7.0.10"} {
		url := "https://example.test/linux-" + version + ".tar.gz"
		entry := lock.FindInput("kernel", version)
		if entry == nil || entry.SHA256 != sha256Hex(responses[url]) {
			t.Fatalf("expected kernel %s pinned to its bytes, got %+v", version, entry)
		}
	}
	if len(lock.Inputs) != 2 || lock.Inputs[0].Version != "7.0.9" || lock.Inputs[1].Version != "7.0.10" {
		t.Fatalf("expected two pins in version order, got %+v", lock.Inputs)
	}
}

// Lint answers a delegated source's inputs from where they were declared: the
// finding names the fetched pekit.toml, and key_files resolve beside it — the
// delegating recipe need not carry a copy of the upstream key.
func TestLintChecksDelegatedInputsWhereDeclared(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	kernel := makeTarGz(t, "linux-7.0.9", "kernel")
	serveURLs(t, map[string][]byte{
		"https://example.test/linux-7.0.9.tar.gz":     kernel,
		"https://example.test/linux-7.0.9.tar.gz.sig": detachSign(t, signer, kernel),
		"https://example.test/firmware-1.0.tar.gz":    makeTarGz(t, "firmware-1.0", "firmware"),
	})
	source := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(source, "pekit.toml"), `
[input.kernel]
url = "https://example.test/linux-{{version}}.tar.gz"
versions = "= 7.0.9"

[input.kernel.signature]
key_files = ["keys/upstream.key"]

[input.firmware]
url = "https://example.test/firmware-{{version}}.tar.gz"
versions = "= 1.0"

[build.main]
command = "true"
`)
	writeFile(t, filepath.Join(source, "lint.pekit.toml"), `
[source]
signature.required = true
signature.keys     = true
`)
	if err := os.MkdirAll(filepath.Join(source, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "keys", "upstream.key"), armoredPublicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"
delegate = true

[source.local]
path = "../src"
`)
	events, err := lintEvents(t, recipe, "lint")
	if diagCode(err) != "lint_failed" {
		t.Fatalf("want lint_failed for the unsigned delegated input, got %v", err)
	}
	rules := lintRules(events["lint"])
	if rules["source.signature.keys"] != 0 {
		t.Fatalf("the source's committed key was looked for in the wrong place: %#v", events["lint"])
	}
	if rules["source.signature.required"] != 1 {
		t.Fatalf("want one unsigned-input finding, got %#v", events["lint"])
	}
	for _, e := range events["lint"] {
		if e.Rule == "source.signature.required" && e.Path != filepath.Join(source, "pekit.toml") {
			t.Fatalf("finding names %s, want the source's pekit.toml", e.Path)
		}
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
