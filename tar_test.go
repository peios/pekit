package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func makeStage(t *testing.T) (job PackageJob) {
	t.Helper()
	root := t.TempDir()
	build := filepath.Join(root, "build", "main")
	out := filepath.Join(root, "package", "main")
	for _, dir := range []string{filepath.Join(build, "bin"), out} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(build, "bin", "app"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, "README"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	return PackageJob{Name: "main", BuildStage: build, OutStage: out}
}

func readTar(t *testing.T, path string) ([]*tar.Header, map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var headers []*tar.Header
	contents := map[string]string{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		headers = append(headers, hdr)
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			contents[hdr.Name] = string(data)
		}
	}
	return headers, contents
}

func TestTarEnginePackagesStage(t *testing.T) {
	job := makeStage(t)
	if err := tarEngine(job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	headers, contents := readTar(t, filepath.Join(job.OutStage, "main.tar"))

	var names []string
	for _, hdr := range headers {
		names = append(names, hdr.Name)
	}
	want := []string{"README", "bin/", "bin/app"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v (lexical order)", names, want)
		}
	}

	if contents["bin/app"] != "binary" || contents["README"] != "docs" {
		t.Errorf("contents = %v", contents)
	}

	for _, hdr := range headers {
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: owner not normalised: uid=%d gid=%d %q %q",
				hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
		}
		if hdr.ModTime.Unix() != 0 {
			t.Errorf("%s: ModTime = %v, want epoch", hdr.Name, hdr.ModTime)
		}
	}

	var app *tar.Header
	for _, hdr := range headers {
		if hdr.Name == "bin/app" {
			app = hdr
		}
	}
	if app.Mode&0o111 == 0 {
		t.Errorf("bin/app lost its exec bit: mode %o", app.Mode)
	}
}

func TestTarEngineIsDeterministic(t *testing.T) {
	job := makeStage(t)
	outPath := filepath.Join(job.OutStage, "main.tar")

	if err := tarEngine(job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	first, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := tarEngine(job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Error("two runs over the same stage produced different bytes")
	}
}
