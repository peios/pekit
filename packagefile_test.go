package main

import (
	"strings"
	"testing"
)

func TestParseMinimalPackageFile(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[package]
name = "loregd"

[files]
"main:loregd" = "usr/bin/loregd"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Format != "tar" || pf.Name != "loregd" {
		t.Errorf("Format=%q Name=%q", pf.Format, pf.Name)
	}
	if len(pf.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(pf.Files))
	}
	want := FileMapping{Source: SourceRef{Target: "main", Path: "loregd"}, Dest: "usr/bin/loregd"}
	if pf.Files[0] != want {
		t.Errorf("Files[0] = %+v, want %+v", pf.Files[0], want)
	}
}

func TestBareColonIsMainSugar(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[package]
name = "loregd"

[files]
":loregd" = "usr/bin/loregd"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := pf.Files[0].Source; got != (SourceRef{Target: "main", Path: "loregd"}) {
		t.Errorf("Source = %+v, want main:loregd", got)
	}
}

func TestPlainPathSourceIsLiteral(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[package]
name = "prelude"

[files]
"target/x86_64-unknown-linux-musl/release/prelude" = "boot/initramfs/init"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	src := pf.Files[0].Source
	if src.Target != "" || src.Path != "target/x86_64-unknown-linux-musl/release/prelude" {
		t.Errorf("Source = %+v, want literal path", src)
	}
}

func TestFilesSortedByDest(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"

[files]
":b" = "usr/share/b"
":a" = "usr/bin/a"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Files[0].Dest != "usr/bin/a" || pf.Files[1].Dest != "usr/share/b" {
		t.Errorf("not sorted by dest: %+v", pf.Files)
	}
}

func TestPackageFileMissingFormat(t *testing.T) {
	_, err := ParsePackageFile(`
[package]
name = "x"

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), `missing required key "format"`) {
		t.Errorf("want missing-format error, got: %v", err)
	}
}

func TestPackageFileMissingName(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), `missing required key "name"`) {
		t.Errorf("want missing-name error, got: %v", err)
	}
}

func TestPackageFileMissingFiles(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "[files] must map at least one file") {
		t.Errorf("want missing-files error, got: %v", err)
	}
}

func TestCutFieldsRejected(t *testing.T) {
	// architecture/description/meta were cut until a format needs them;
	// accepting them silently would be a compat promise nobody made.
	_, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"
architecture = "x86_64"

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), `unknown key "architecture"`) {
		t.Errorf("want unknown-key error, got: %v", err)
	}

	_, err = ParsePackageFile(`
format = "tar"

[meta]
license = "MIT"
`)
	if err == nil || !strings.Contains(err.Error(), `unknown key "meta"`) {
		t.Errorf("want unknown-key error for [meta], got: %v", err)
	}
}

func TestEmptyStagePathRejected(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"

[files]
"main:" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), "names no file") {
		t.Errorf("want empty-stage-path error, got: %v", err)
	}
}

func TestEscapingSourceRejected(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"

[files]
"main:../../secrets" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), "stay inside the stage") {
		t.Errorf("want stage-escape error, got: %v", err)
	}
}

func TestBadDestsRejected(t *testing.T) {
	for _, dest := range []string{"/usr/bin/x", "..", "../x", "."} {
		_, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"

[files]
":x" = "` + dest + `"
`)
		if err == nil || !strings.Contains(err.Error(), "relative path inside the package") {
			t.Errorf("dest %q: want bad-dest error, got: %v", dest, err)
		}
	}
}

func TestDuplicateDestRejected(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"

[package]
name = "x"

[files]
":a" = "usr/bin/x"
":b" = "usr/bin//x"
`)
	if err == nil || !strings.Contains(err.Error(), "both map to") {
		t.Errorf("want duplicate-dest error, got: %v", err)
	}
}

func TestUnrecognisedFormatError(t *testing.T) {
	_, err := engineFor("peipkg")
	if err == nil || !strings.Contains(err.Error(), `unrecognised package format "peipkg"`) {
		t.Errorf("want unrecognised-format error, got: %v", err)
	}
}
