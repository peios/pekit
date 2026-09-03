package pekit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type testPyPIFile struct {
	Filename string
	URL      string
	Body     []byte
	Yanked   any
	Hash     string
}

func servePyPIProject(t *testing.T, project string, files []testPyPIFile, indexCalls *int) {
	t.Helper()
	project = normalizePyPIProject(project)
	entries := make([]map[string]any, 0, len(files))
	artifacts := map[string][]byte{}
	for _, file := range files {
		hash := file.Hash
		if hash == "" && file.Body != nil {
			sum := sha256.Sum256(file.Body)
			hash = hex.EncodeToString(sum[:])
		}
		yanked := file.Yanked
		if yanked == nil {
			yanked = false
		}
		entries = append(entries, map[string]any{
			"filename": file.Filename,
			"url":      file.URL,
			"hashes":   map[string]string{"sha256": hash},
			"yanked":   yanked,
		})
		if file.Body != nil {
			artifacts[file.URL] = file.Body
		}
	}
	body, err := json.Marshal(map[string]any{
		"meta":  map[string]string{"api-version": "1.1"},
		"name":  project,
		"files": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	indexURL := pypiSimpleBaseURL + project + "/"
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case indexURL:
			*indexCalls++
			if req.Header.Get("Accept") != "application/vnd.pypi.simple.v1+json" {
				return nil, fmt.Errorf("unexpected Accept header %q", req.Header.Get("Accept"))
			}
			header := make(http.Header)
			header.Set("Content-Type", "application/vnd.pypi.simple.v1+json; charset=utf-8")
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: header, Request: req}, nil
		default:
			artifact, ok := artifacts[req.URL.String()]
			if !ok {
				return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header), Request: req}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(artifact)), Header: make(http.Header), Request: req}, nil
		}
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
}

func TestPyPILatestPackagesSdistAndReplaysLockWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	oneURL := "https://files.example.test/demo_pkg-1.0.0.tar.gz"
	twoURL := "https://files.example.test/demo_pkg-2.0.0.tar.gz"
	one := makeTarGz(t, "demo_pkg-1.0.0", "one")
	two := makeTarGz(t, "demo_pkg-2.0.0", "two")
	indexCalls := 0
	servePyPIProject(t, "demo-pkg", []testPyPIFile{
		{Filename: "demo_pkg-1.0.0.tar.gz", URL: oneURL, Body: one},
		{Filename: "demo_pkg-2.0.0.tar.gz", URL: twoURL, Body: two},
		{Filename: "demo_pkg-3.0.0.tar.gz", URL: "https://files.example.test/demo_pkg-3.0.0.tar.gz", Body: makeTarGz(t, "demo_pkg-3.0.0", "three"), Yanked: "broken"},
		{Filename: "demo_pkg-4.0.0rc1.tar.gz", URL: "https://files.example.test/demo_pkg-4.0.0rc1.tar.gz", Body: makeTarGz(t, "demo_pkg-4.0.0rc1", "prerelease")},
		{Filename: "demo_pkg-5.0.0-py3-none-any.whl", URL: "https://files.example.test/demo_pkg-5.0.0-py3-none-any.whl", Body: []byte("wheel")},
	}, &indexCalls)
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.pypi]
project = "Demo_Pkg"
artifact = "sdist"
versions = ">= 1.0.0"

[build]
command = 'cat payload.txt > "$PEKIT_OUT/value"'
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
name = "example.demo"
version = "{{version}}-1"
architecture = "noarch"
description = "PyPI test package"
license = "MIT"

[files]
":value" = "usr/share/value"
`)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--latest"}); err != nil {
		t.Fatalf("package latest failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if indexCalls != 1 {
		t.Fatalf("index requests = %d, want 1", indexCalls)
	}
	lock, err := LoadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("2.0.0")
	if entry == nil || entry.URL != twoURL || entry.SHA256 != sha256Bytes(two) {
		t.Fatalf("unexpected PyPI lock entry: %#v", entry)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "out", "*", "package", "*", "*-source_2.0.0-1_noarch.peipkg")); len(matches) != 1 {
		t.Fatalf("expected corresponding source package, got %v", matches)
	}
	manifestPaths, err := filepath.Glob(filepath.Join(dir, "out", "url-*", "source.pekit.json"))
	if err != nil || len(manifestPaths) != 1 {
		t.Fatalf("expected one source manifest, got %v, %v", manifestPaths, err)
	}
	manifestData, err := os.ReadFile(manifestPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var manifest SourceManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != "pypi" || !strings.HasPrefix(manifest.ProvenanceRef, "pypi:demo-pkg@2.0.0#sha256:") {
		t.Fatalf("unexpected PyPI source manifest: %#v", manifest)
	}

	// An exact locked rebuild must not need the project index. Refreshing the
	// source also proves the pinned file URL and hash are sufficient to fetch
	// and validate the artifact again.
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != twoURL {
			return nil, fmt.Errorf("offline index: unexpected request %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(two)), Header: make(http.Header), Request: req}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	stdout.Reset()
	stderr.Reset()
	if err := app.Run([]string{"build", "--version", "2.0.0", "--refresh-source"}); err != nil {
		t.Fatalf("offline locked rebuild failed: %v\nstderr=%s", err, stderr.String())
	}
	valueFiles, err := filepath.Glob(filepath.Join(dir, "out", "url-*", "build", "main", "value"))
	if err != nil || len(valueFiles) == 0 {
		t.Fatalf("missing build output: %v, %v", valueFiles, err)
	}
	value, err := os.ReadFile(valueFiles[len(valueFiles)-1])
	if err != nil || string(value) != "two" {
		t.Fatalf("build output = %q, %v", value, err)
	}
}

func TestPyPIEnumerationExcludesYankedPrereleaseAndWheels(t *testing.T) {
	indexCalls := 0
	servePyPIProject(t, "demo", []testPyPIFile{
		{Filename: "demo-1.0.0.tar.gz", URL: "https://files.example.test/demo-1.0.0.tar.gz", Body: []byte("one")},
		{Filename: "demo-2.0.0.tar.gz", URL: "https://files.example.test/demo-2.0.0.tar.gz", Body: []byte("two"), Yanked: true},
		{Filename: "demo-3.0.0rc1.tar.gz", URL: "https://files.example.test/demo-3.0.0rc1.tar.gz", Body: []byte("three")},
		{Filename: "demo-4.0.0-py3-none-any.whl", URL: "https://files.example.test/demo-4.0.0-py3-none-any.whl", Body: []byte("four")},
	}, &indexCalls)
	ctx := &Context{}
	versions, err := enumeratePyPIVersions(ctx, PyPISourceConfig{Project: "demo", Artifact: "sdist"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions, ","); got != "1.0.0" {
		t.Fatalf("enumerated versions = %q, want 1.0.0", got)
	}
	if _, err := enumeratePyPIVersions(ctx, PyPISourceConfig{Project: "demo", Artifact: "sdist"}); err != nil {
		t.Fatal(err)
	}
	if indexCalls != 1 {
		t.Fatalf("cached index requests = %d, want 1", indexCalls)
	}
}

func TestPyPIAmbiguousSdistFails(t *testing.T) {
	indexCalls := 0
	body := makeTarGz(t, "demo-1.0.0", "payload")
	servePyPIProject(t, "demo", []testPyPIFile{
		{Filename: "demo-1.0.0.tar.gz", URL: "https://one.example.test/demo-1.0.0.tar.gz", Body: body},
		{Filename: "demo-1.0.0.tar.gz", URL: "https://two.example.test/demo-1.0.0.tar.gz", Body: body},
	}, &indexCalls)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.pypi]
project = "demo"
artifact = "sdist"

[build]
command = "true"
`)
	chdir(t, dir)
	app := &App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.Run([]string{"build", "--version", "1.0.0"})
	if diagCode(err) != "pypi_sdist_ambiguous" {
		t.Fatalf("expected pypi_sdist_ambiguous, got %v", err)
	}
}

func TestPyPINoEligibleVersionsFailsLatest(t *testing.T) {
	indexCalls := 0
	servePyPIProject(t, "demo", []testPyPIFile{
		{Filename: "demo-1.0.0.tar.gz", URL: "https://files.example.test/demo-1.0.0.tar.gz", Body: []byte("one"), Yanked: true},
	}, &indexCalls)
	ctx := &Context{Inv: Invocation{Latest: true}}
	_, err := ResolveVersions(ctx, SourceConfig{PyPI: PyPISourceConfig{Project: "demo", Artifact: "sdist"}})
	if diagCode(err) != "version_selection_empty" {
		t.Fatalf("expected version_selection_empty, got %v", err)
	}
}

func TestPyPIEligibleSdistRequiresSHA256(t *testing.T) {
	indexCalls := 0
	servePyPIProject(t, "demo", []testPyPIFile{
		{Filename: "demo-1.0.0.tar.gz", URL: "https://files.example.test/demo-1.0.0.tar.gz", Hash: "missing"},
	}, &indexCalls)
	_, err := enumeratePyPIVersions(&Context{}, PyPISourceConfig{Project: "demo", Artifact: "sdist"})
	if diagCode(err) != "pypi_index" {
		t.Fatalf("expected pypi_index, got %v", err)
	}
}

func TestParsePyPISDistFilename(t *testing.T) {
	tests := map[string]bool{
		"demo_pkg-1.2.3.tar.gz":           true,
		"demo_pkg-1.2.3rc1.tar.gz":        false,
		"other-1.2.3.tar.gz":              false,
		"demo_pkg-1.2.3-py3-none-any.whl": false,
		"legacy-demo-pkg-1.2.3.tar.gz":    false,
	}
	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _, got := parsePyPISDistFilename("demo-pkg", name)
		if got != tests[name] {
			t.Errorf("parsePyPISDistFilename(%q) eligible = %v, want %v", name, got, tests[name])
		}
	}
}

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
