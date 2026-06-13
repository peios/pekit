package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseMinimalPackageFile(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[files]
"main:loregd" = "usr/bin/loregd"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Format != "tar" {
		t.Errorf("Format = %q", pf.Format)
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
[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), `missing required key "format"`) {
		t.Errorf("want missing-format error, got: %v", err)
	}
}

func TestPackageNameOverride(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[package]
name = "libp"

[files]
":x" = "usr/bin/x"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Name != "libp" {
		t.Errorf("Name = %q, want %q", pf.Name, "libp")
	}
}

func TestPackageNameDefaultsEmpty(t *testing.T) {
	// No [package] -> Name empty; the caller falls back to the
	// project directory name.
	pf, err := ParsePackageFile(`
format = "tar"

[files]
":x" = "usr/bin/x"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Name != "" {
		t.Errorf("Name = %q, want empty", pf.Name)
	}
}

func TestPackageFileMissingFiles(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"
`)
	if err == nil || !strings.Contains(err.Error(), "[files] must map at least one file") {
		t.Errorf("want missing-files error, got: %v", err)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"

[package]
arch = "x86_64"

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), `unknown key "arch"`) {
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

[files]
":a" = "usr/bin/x"
":b" = "usr/bin//x"
`)
	if err == nil || !strings.Contains(err.Error(), "both map to") {
		t.Errorf("want duplicate-dest error, got: %v", err)
	}
}

func TestUnrecognisedFormatError(t *testing.T) {
	_, err := engineFor("rpm")
	if err == nil || !strings.Contains(err.Error(), `unrecognised package format "rpm"`) {
		t.Errorf("want unrecognised-format error, got: %v", err)
	}
}

func TestKitchenSinkExampleParses(t *testing.T) {
	// The design-sketch example must stay parse-clean as the format grows.
	data, err := os.ReadFile("examples/package.pekit.toml")
	if err != nil {
		t.Fatal(err)
	}
	pf, err := ParsePackageFile(string(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.Name != "jellyfind" || pf.Version != "2.4.1" || pf.Architecture != "x86_64" {
		t.Errorf("identity = %q %q %q", pf.Name, pf.Version, pf.Architecture)
	}
	if len(pf.Dependencies) != 3 || len(pf.Files) != 5 || len(pf.SDOverrides) != 2 {
		t.Errorf("deps=%d files=%d sdOverrides=%d", len(pf.Dependencies), len(pf.Files), len(pf.SDOverrides))
	}
}

func TestDependencyForms(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "peipkg"

[dependencies]
loregd  = ">= 0.3.0"
eventd  = "*"
libkacs = { constraint = ">= 1.2", arch = "x86_64" }

[files]
":x" = "usr/bin/x"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Dependency{
		{Name: "eventd", Constraint: ""},
		{Name: "libkacs", Constraint: ">= 1.2", Arch: "x86_64"},
		{Name: "loregd", Constraint: ">= 0.3.0"},
	}
	if len(pf.Dependencies) != len(want) {
		t.Fatalf("deps = %+v", pf.Dependencies)
	}
	for i := range want {
		if pf.Dependencies[i] != want[i] {
			t.Errorf("deps[%d] = %+v, want %+v", i, pf.Dependencies[i], want[i])
		}
	}
}

func TestEmptyConstraintRejected(t *testing.T) {
	_, err := ParsePackageFile(`
format = "peipkg"

[dependencies]
loregd = ""

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), `use "*" for any version`) {
		t.Errorf("want any-version-spelling error, got: %v", err)
	}
}

func TestSideEffectsOrderPreserved(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "peipkg"

[package]
sideEffects = ["zeta", "alpha"]

[files]
":x" = "usr/bin/x"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pf.SideEffects) != 2 || pf.SideEffects[0] != "zeta" || pf.SideEffects[1] != "alpha" {
		t.Errorf("SideEffects = %v, want document order", pf.SideEffects)
	}
}

func TestSDOverridePathValidated(t *testing.T) {
	_, err := ParsePackageFile(`
format = "peipkg"

[sdOverrides]
"/usr/bin/x" = "O:SY"

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Errorf("want path-validation error, got: %v", err)
	}
}

func TestTarRejectsManifestFields(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"

[package]
version = "1.0"

[dependencies]
loregd = "*"

[files]
":x" = "usr/bin/x"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = tarEngine(PackageJob{Pkg: pf, Name: "x", OutStage: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "format tar cannot express version, dependencies") {
		t.Errorf("want tar-cannot-express error, got: %v", err)
	}
}

func TestBuildsUnionsWithDerived(t *testing.T) {
	pf, err := ParsePackageFile(`
format = "tar"
builds = ["main", "tools"]

[files]
"cli:x" = "usr/bin/x"
"target/release/y" = "usr/bin/y"
`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := referencedBuildTargets(pf)
	want := []string{"cli", "main", "tools"}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %v, want %v (derived ∪ declared, sorted)", got, want)
		}
	}
}

func TestEmptyBuildsRejected(t *testing.T) {
	_, err := ParsePackageFile(`
format = "tar"
builds = []

[files]
":x" = "usr/bin/x"
`)
	if err == nil || !strings.Contains(err.Error(), "does nothing") {
		t.Errorf("want empty-builds error, got: %v", err)
	}
}

// --- per-section delegation merge (recipe over source) ---

func TestMergeFieldLevelPackage(t *testing.T) {
	// Recipe overrides only [package].description; everything else —
	// format, version, architecture, [files] — comes from the source.
	source := map[string]any{
		"format": "peipkg",
		"package": map[string]any{
			"version":      "0.21.2-1",
			"architecture": "x86_64",
			"description":  "upstream desc",
		},
		"files": map[string]any{":loregd": "usr/bin/loregd"},
	}
	recipe := map[string]any{
		"package": map[string]any{"description": "farm desc"},
	}
	pf, err := parsePackageRaw(mergePackageRaw(recipe, source))
	if err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	if pf.Format != "peipkg" {
		t.Errorf("Format = %q, want peipkg (source)", pf.Format)
	}
	if pf.Version != "0.21.2-1" || pf.Architecture != "x86_64" {
		t.Errorf("version/arch = %q/%q, want source's", pf.Version, pf.Architecture)
	}
	if pf.Description != "farm desc" {
		t.Errorf("Description = %q, want recipe override", pf.Description)
	}
	if len(pf.Files) != 1 || pf.Files[0].Dest != "usr/bin/loregd" {
		t.Errorf("Files = %+v, want source's", pf.Files)
	}
}

func TestMergeWholeUnitFilesAndFormat(t *testing.T) {
	source := map[string]any{
		"format":  "peipkg",
		"package": map[string]any{"version": "1.0.0-1", "architecture": "x86_64"},
		"files":   map[string]any{":app": "usr/bin/app"},
	}
	// Recipe replaces [files] wholesale and overrides format.
	recipe := map[string]any{
		"format": "tar",
		"files":  map[string]any{":app": "usr/sbin/app"},
	}
	pf, err := parsePackageRaw(mergePackageRaw(recipe, source))
	if err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	if pf.Format != "tar" {
		t.Errorf("Format = %q, want tar (recipe whole-unit)", pf.Format)
	}
	if len(pf.Files) != 1 || pf.Files[0].Dest != "usr/sbin/app" {
		t.Errorf("Files = %+v, want recipe's usr/sbin/app", pf.Files)
	}
}

func TestMergeNilSides(t *testing.T) {
	src := map[string]any{"format": "peipkg"}
	if got := mergePackageRaw(nil, src); got["format"] != "peipkg" {
		t.Error("nil recipe should yield source")
	}
	rec := map[string]any{"format": "tar"}
	if got := mergePackageRaw(rec, nil); got["format"] != "tar" {
		t.Error("nil source should yield recipe")
	}
	if mergePackageRaw(nil, nil) != nil {
		t.Error("both nil should yield nil")
	}
}

func TestMergeDoesNotMutateSource(t *testing.T) {
	srcPkg := map[string]any{"description": "upstream"}
	source := map[string]any{"package": srcPkg}
	recipe := map[string]any{"package": map[string]any{"description": "recipe"}}
	_ = mergePackageRaw(recipe, source)
	if srcPkg["description"] != "upstream" {
		t.Errorf("source [package] was mutated: %v", srcPkg["description"])
	}
}
