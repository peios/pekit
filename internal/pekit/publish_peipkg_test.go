package pekit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePeipkgPublishTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.pekit.toml")
	writeFile(t, path, `
[publish.peipkg]
path = "dist/repository"
name = "experimental"
signing_key = "keyring:signing.repository_key"
`)
	cfg, err := LoadPackageFile(path)
	if err != nil {
		t.Fatalf("load package: %v", err)
	}
	if cfg.Publish.Peipkg == nil {
		t.Fatal("publish.peipkg was not parsed")
	}
	got := *cfg.Publish.Peipkg
	if got.Path != "dist/repository" || got.Name != "experimental" ||
		got.SigningKey != "keyring:signing.repository_key" {
		t.Fatalf("publish.peipkg = %#v", got)
	}
}

func TestPublishPeipkgInitializesRepositoryAndBatchesArtifacts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	for _, name := range []string{"alpha", "beta"} {
		writeFile(t, filepath.Join(dir, name+".txt"), name)
		writeFile(t, filepath.Join(dir, name+".package.pekit.toml"), `
format = "peipkg"

[package]
name = "`+name+`"
version = "1.0.0-1"
architecture = "x86_64"
description = "test package"
license = "MIT"

[files]
"@recipe:`+name+`.txt" = "usr/share/`+name+`/payload.txt"

[publish.peipkg]
path = "repo"
name = "test-repository"
signing_key = "keyring:signing.repository_key"
`)
	}
	packageKeyDir := filepath.Join(dir, "package-key")
	repositoryKeyDir := filepath.Join(dir, "repository-key")
	if err := os.MkdirAll(packageKeyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repositoryKeyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packageKey, _ := writeSeedKey(t, packageKeyDir)
	repositoryKey, _ := writeSeedKey(t, repositoryKeyDir)
	chdir(t, dir)

	var stdout, stderr bytes.Buffer
	app := &App{
		Stdout: &stdout,
		Stderr: &stderr,
		Now: func() time.Time {
			return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
		},
	}
	err := app.Run([]string{
		"publish", "--all",
		"--keyring.signing.package_key=" + packageKey,
		"--keyring.signing.repository_key=" + repositoryKey,
	})
	if err != nil {
		t.Fatalf("publish failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}

	repo := filepath.Join(dir, "repo")
	for _, rel := range []string{
		".peipkg-repo.json", "repo.json", "repo.json.sig",
		"index/active.json", "index/active.json.sig",
		"index/archive.json", "index/archive.json.sig",
		"p/alpha/1.0.0-1/alpha_1.0.0-1_x86_64.peipkg",
		"p/beta/1.0.0-1/beta_1.0.0-1_x86_64.peipkg",
	} {
		if !fileExists(filepath.Join(repo, filepath.FromSlash(rel))) {
			t.Errorf("repository is missing %s", rel)
		}
	}

	archive := readPublishedIndex(t, filepath.Join(repo, "index", "archive.json"))
	if archive.Repo != "test-repository" || archive.Kind != "archive" ||
		archive.IndexVersion != 2 || len(archive.Packages) != 2 {
		t.Fatalf("archive index = %#v", archive)
	}
	active := readPublishedIndex(t, filepath.Join(repo, "index", "active.json"))
	if active.IndexVersion != 2 || len(active.Packages) != 2 {
		t.Fatalf("active index = %#v", active)
	}
	if got := strings.Count(stdout.String(), "index_version 2"); got != 2 {
		t.Errorf("publish events at batch index version = %d, want 2\n%s", got, stdout.String())
	}

	var descriptor struct {
		Repo struct {
			Name    string `json:"name"`
			Signing struct {
				Keys []json.RawMessage `json:"keys"`
			} `json:"signing"`
		} `json:"repo"`
	}
	readJSONFile(t, filepath.Join(repo, "repo.json"), &descriptor)
	if descriptor.Repo.Name != "test-repository" || len(descriptor.Repo.Signing.Keys) != 2 {
		t.Fatalf("descriptor = %#v", descriptor)
	}
}

func TestPublishPeipkgDirectKeyPathAndDefaultName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	definition := `
format = "peipkg"

[package]
version = "1.0.0-1"
architecture = "x86_64"
description = "test package"
license = "MIT"

[files]
"@recipe:payload.txt" = "usr/share/test/payload.txt"

[publish.peipkg]
path = "my-repository"
signing_key = "repository.key"
`
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), definition)
	keyPath, _ := writeSeedKey(t, dir)
	if err := os.Rename(keyPath, filepath.Join(dir, "repository.key")); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{
		"publish", "--keyring.signing.package_key=" + filepath.Join(dir, "repository.key"),
	})
	if err != nil {
		t.Fatalf("publish failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	var descriptor struct {
		Repo struct {
			Name string `json:"name"`
		} `json:"repo"`
	}
	readJSONFile(t, filepath.Join(dir, "my-repository", "repo.json"), &descriptor)
	if descriptor.Repo.Name != "my-repository" {
		t.Fatalf("default repository name = %q", descriptor.Repo.Name)
	}

	// A later version updates the existing repository rather than attempting
	// to initialize it again. The old version remains in the archive.
	writeFile(t, filepath.Join(dir, "package.pekit.toml"),
		strings.Replace(definition, "1.0.0-1", "1.0.1-1", 1))
	if err := app.Run([]string{
		"publish", "--keyring.signing.package_key=" + filepath.Join(dir, "repository.key"),
	}); err != nil {
		t.Fatalf("incremental publish failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	archive := readPublishedIndex(t, filepath.Join(dir, "my-repository", "index", "archive.json"))
	active := readPublishedIndex(t, filepath.Join(dir, "my-repository", "index", "active.json"))
	if archive.IndexVersion != 3 || len(archive.Packages) != 2 ||
		active.IndexVersion != 3 || len(active.Packages) != 1 {
		t.Fatalf("indexes after incremental publish: archive=%#v active=%#v", archive, active)
	}
}

func TestPublishPeipkgRejectsNonPeipkgArtifact(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[files]
"@recipe:payload.txt" = "usr/share/test/payload.txt"

[publish.peipkg]
path = "repo"
signing_key = "repository.key"
`)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"publish"}); diagCode(err) != "invalid_publish_target" {
		t.Fatalf("expected invalid_publish_target, got %v", err)
	}
	if dirExists(filepath.Join(dir, "out")) || dirExists(filepath.Join(dir, "repo")) {
		t.Fatal("invalid repository target performed build or publish work")
	}
}

func TestWorkspacePublishPeipkgSerializesSharedRepository(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "workspace.pekit.toml"), `include = ["./*"]`)
	for _, name := range []string{"alpha", "beta"} {
		member := filepath.Join(dir, name)
		writeFile(t, filepath.Join(member, "pekit.toml"), `out_dir = "out"`)
		writeFile(t, filepath.Join(member, "payload.txt"), name)
		writeFile(t, filepath.Join(member, "package.pekit.toml"), `
format = "peipkg"

[package]
name = "`+name+`"
version = "1.0.0-1"
architecture = "x86_64"
description = "test package"
license = "MIT"

[files]
"@recipe:payload.txt" = "usr/share/`+name+`/payload.txt"

[publish.peipkg]
path = "repo"
name = "workspace-repository"
signing_key = "keyring:signing.repository_key"
`)
	}
	keyPath, _ := writeSeedKey(t, dir)
	chdir(t, dir)

	var stdout, stderr bytes.Buffer
	app := &App{
		Stdout: &stdout,
		Stderr: &stderr,
		Now: func() time.Time {
			return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
		},
	}
	err := app.Run([]string{
		"workspace", "--jobs", "2", "publish",
		"--keyring.signing.package_key=" + keyPath,
		"--keyring.signing.repository_key=" + keyPath,
	})
	if err != nil {
		t.Fatalf("workspace publish failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	index := readPublishedIndex(t, filepath.Join(dir, "repo", "index", "archive.json"))
	if index.IndexVersion != 3 || len(index.Packages) != 2 {
		t.Fatalf("archive index after two serialized publishes = %#v", index)
	}
}

func TestPublishPeipkgMissingKeyringEntryFailsBeforeWritingArtifact(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `out_dir = "out"`)
	writeFile(t, filepath.Join(dir, "payload.txt"), "payload")
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
version = "1.0.0-1"
architecture = "x86_64"
description = "test package"
license = "MIT"

[files]
"@recipe:payload.txt" = "usr/share/test/payload.txt"

[publish.peipkg]
path = "repo"
signing_key = "keyring:signing.repository_key"
`)
	packageKey, _ := writeSeedKey(t, dir)
	chdir(t, dir)

	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run([]string{"publish", "--keyring.signing.package_key=" + packageKey})
	if diagCode(err) != "repository_signing_key" {
		t.Fatalf("expected repository_signing_key, got %v", err)
	}
	if len(globAll(t, filepath.Join(dir, "out", "package", "*", "*.peipkg"))) != 0 {
		t.Fatal("package artifact was written before repository signing-key validation")
	}
	if dirExists(filepath.Join(dir, "repo")) {
		t.Fatal("repository was initialized without its signing key")
	}
}

type publishedIndex struct {
	Repo         string            `json:"repo"`
	Kind         string            `json:"kind"`
	IndexVersion int64             `json:"index_version"`
	Packages     []json.RawMessage `json:"packages"`
}

func readPublishedIndex(t *testing.T, path string) publishedIndex {
	t.Helper()
	var index publishedIndex
	readJSONFile(t, path, &index)
	return index
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func globAll(t *testing.T, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
