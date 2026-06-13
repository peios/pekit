package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func makeJob(t *testing.T) PackageJob {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "loregd")
	doc := filepath.Join(root, "README")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out", "package", "loregd")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return PackageJob{
		Pkg:  &PackageFile{Format: "tar"},
		Name: "loregd",
		Files: []StagedFile{
			{Source: bin, Dest: "usr/bin/loregd"},
			{Source: doc, Dest: "usr/share/doc/loregd/README"},
		},
		OutStage: out,
	}
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

func TestTarEnginePackagesFiles(t *testing.T) {
	job := makeJob(t)
	if _, err := tarEngine(job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	headers, contents := readTar(t, filepath.Join(job.OutStage, "loregd.tar"))

	var names []string
	for _, hdr := range headers {
		names = append(names, hdr.Name)
	}
	want := []string{
		"usr/", "usr/bin/", "usr/share/", "usr/share/doc/", "usr/share/doc/loregd/",
		"usr/bin/loregd", "usr/share/doc/loregd/README",
	}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}

	if contents["usr/bin/loregd"] != "binary" || contents["usr/share/doc/loregd/README"] != "docs" {
		t.Errorf("contents = %v", contents)
	}

	for _, hdr := range headers {
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s: owner not normalised", hdr.Name)
		}
		if hdr.ModTime.Unix() != 0 {
			t.Errorf("%s: ModTime = %v, want epoch", hdr.Name, hdr.ModTime)
		}
	}

	for _, hdr := range headers {
		if hdr.Name == "usr/bin/loregd" && hdr.Mode&0o111 == 0 {
			t.Errorf("usr/bin/loregd lost its exec bit: mode %o", hdr.Mode)
		}
	}
}

func TestTarEngineIsDeterministic(t *testing.T) {
	job := makeJob(t)
	outPath := filepath.Join(job.OutStage, "loregd.tar")

	if _, err := tarEngine(job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	first, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tarEngine(job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Error("two runs over the same inputs produced different bytes")
	}
}
