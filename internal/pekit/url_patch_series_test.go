package pekit

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const urlPatchSeriesRecipe = `
out_dir = "out"

[source.url]
url = "https://example.test/releases/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"
versions = ">= 1.2.0"
file_regex = 'app-[0-9]+\.[0-9]+\.tar\.gz'

[source.url.signature]
key_files = ["keys/upstream.key"]

[source.url.patch_series]
url = "https://example.test/releases/app-{{major}}.{{minor}}-patches/app{{major}}{{minor}}-{{patch}}"
patch_width = 3
strip = 1

[source.url.patch_series.signature]
key_files = ["keys/upstream.key"]

[source_package]
name = "app-source"

[build]
command = "cp payload.txt \"$PEKIT_OUT/payload.txt\""
`

const urlPatchSeriesPackage = `
format = "peipkg"

[package]
name = "app"
version = "{{version}}-1"
architecture = "noarch"
license = "MIT"

[files]
":payload.txt" = "usr/share/app/payload.txt"
`

const patchOne = `--- a/payload.txt
+++ b/payload.txt
@@ -1 +1 @@
-base
+patch one
`

const patchTwo = `--- a/payload.txt
+++ b/payload.txt
@@ -1 +1 @@
-patch one
+patch two
`

func TestURLPatchSeriesEnumeratesBuildsLocksAndShipsSource(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	base := makeTarGz(t, "app-1.2", "base\n")
	responses := map[string][]byte{
		"https://example.test/releases/":                              []byte(`<a href="app-1.2.tar.gz">app-1.2.tar.gz</a>`),
		"https://example.test/releases/app-1.2-patches/":              []byte(`<a href="app12-001">app12-001</a><a href="app12-001.sig">sig</a><a href="app12-002">app12-002</a>`),
		"https://example.test/releases/app-1.2.tar.gz":                base,
		"https://example.test/releases/app-1.2.tar.gz.sig":            detachSign(t, signer, base),
		"https://example.test/releases/app-1.2-patches/app12-001":     []byte(patchOne),
		"https://example.test/releases/app-1.2-patches/app12-001.sig": detachSign(t, signer, []byte(patchOne)),
		"https://example.test/releases/app-1.2-patches/app12-002":     []byte(patchTwo),
		"https://example.test/releases/app-1.2-patches/app12-002.sig": detachSign(t, signer, []byte(patchTwo)),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), urlPatchSeriesRecipe)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), urlPatchSeriesPackage)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--latest"}); err != nil {
		t.Fatalf("package failed: %v\nstderr=%s", err, stderr.String())
	}

	artifact := globOne(t, filepath.Join(dir, "out", "*", "package", "*", "app_1.2.2-1_noarch.peipkg"))
	if got := string(readPeipkgEntries(t, artifact)["usr/share/app/payload.txt"]); got != "patch two\n" {
		t.Fatalf("packaged payload = %q, want final patch content", got)
	}
	lock, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("1.2.2")
	wantFpr := hex.EncodeToString(signer.PrimaryKey.Fingerprint)
	if entry == nil || entry.SignatureKey != wantFpr || len(entry.Patches) != 2 {
		t.Fatalf("unexpected composite lock entry: %+v", entry)
	}
	for i, patch := range entry.Patches {
		if patch.SignatureKey != wantFpr || patch.SHA256 == "" {
			t.Fatalf("patch lock %d = %+v", i+1, patch)
		}
	}

	sourceArtifact := globOne(t, filepath.Join(dir, "out", "*", "package", "*", "app-source_1.2.2-1_noarch.peipkg"))
	entries := readPeipkgEntries(t, sourceArtifact)
	for _, suffix := range []string{"/upstream/app-1.2.tar.gz", "/upstream/patches/app12-001", "/upstream/patches/app12-002"} {
		found := false
		for name := range entries {
			if strings.HasSuffix(name, suffix) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("source package is missing %s", suffix)
		}
	}

	// A changed patch cannot be hidden behind a still-valid base archive.
	responses["https://example.test/releases/app-1.2-patches/app12-002"] = []byte(strings.ReplaceAll(patchTwo, "patch two", "tampered"))
	err = app.Run([]string{"build", "--version", "1.2.2", "--refresh-source"})
	if diagCode(err) != "lock_mismatch" {
		t.Fatalf("expected changed patch to fail with lock_mismatch, got %v", err)
	}
}

func TestURLPatchSeriesRejectsGap(t *testing.T) {
	responses := map[string][]byte{
		"https://example.test/releases/":                 []byte(`<a href="app-1.2.tar.gz">app-1.2.tar.gz</a>`),
		"https://example.test/releases/app-1.2-patches/": []byte(`<a href="app12-001">app12-001</a><a href="app12-003">app12-003</a>`),
	}
	serveURLs(t, responses)
	cfg := URLSourceConfig{
		URL:       "https://example.test/releases/app-{{version}}.tar.gz",
		FileRegex: `app-[0-9]+\.[0-9]+\.tar\.gz`,
		Versions:  ">= 1.2.0",
		PatchSeries: URLPatchSeriesConfig{
			URL:        "https://example.test/releases/app-{{major}}.{{minor}}-patches/app{{major}}{{minor}}-{{patch}}",
			PatchWidth: 3,
		},
	}
	_, err := enumerateURLVersions(cfg)
	if diagCode(err) != "url_patch_gap" {
		t.Fatalf("expected url_patch_gap, got %v", err)
	}
}

func TestURLPatchSeriesRequiresExplicitPatchComponent(t *testing.T) {
	cfg := URLSourceConfig{PatchSeries: URLPatchSeriesConfig{URL: "https://example.test/app-{{patch}}"}}
	version, err := ParseVersion("1.2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := urlBaseVersion(cfg, version); diagCode(err) != "invalid_patch_series_version" {
		t.Fatalf("expected invalid_patch_series_version, got %v", err)
	}
}

func TestURLPatchSeriesInvalidSignatureWritesNoLock(t *testing.T) {
	dir := t.TempDir()
	signer := newTestSigner(t)
	imposter := newTestSigner(t)
	base := makeTarGz(t, "app-1.2", "base\n")
	responses := map[string][]byte{
		"https://example.test/releases/":                              []byte(`<a href="app-1.2.tar.gz">app-1.2.tar.gz</a>`),
		"https://example.test/releases/app-1.2-patches/":              []byte(`<a href="app12-001">app12-001</a>`),
		"https://example.test/releases/app-1.2.tar.gz":                base,
		"https://example.test/releases/app-1.2.tar.gz.sig":            detachSign(t, signer, base),
		"https://example.test/releases/app-1.2-patches/app12-001":     []byte(patchOne),
		"https://example.test/releases/app-1.2-patches/app12-001.sig": detachSign(t, imposter, []byte(patchOne)),
	}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "pekit.toml"), urlPatchSeriesRecipe)
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "upstream.key"), publicKeyBytes(t, signer), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.Run([]string{"build", "--version", "1.2.1"})
	if diagCode(err) != "signature_invalid" {
		t.Fatalf("expected signature_invalid, got %v", err)
	}
	lock, loadErr := LoadLockFile(dir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(lock.Sources) != 0 {
		t.Fatalf("invalid patch signature wrote lock entries: %+v", lock.Sources)
	}
}
