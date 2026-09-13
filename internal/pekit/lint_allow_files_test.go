package pekit

import (
	"path/filepath"
	"testing"
)

func TestLintAllowFilesValidation(t *testing.T) {
	for _, tc := range []struct{ name, input, code string }{
		{"blank-reason", "[allow_files.\"payload.junk\"]\n\"usr/lib/start.o\" = \" \"", "missing_reason"},
		{"unknown-rule", "[allow_files.\"payload.typo\"]\n\"usr/lib/start.o\" = \"reason\"", "unknown_key"},
		{"parameter", "[allow_files.\"payload.dirs.lib\"]\n\"usr/lib\" = \"reason\"", "unknown_key"},
		{"static-rule", "[allow_files.\"package.license\"]\n\"usr/lib/start.o\" = \"reason\"", "unknown_key"},
		{"absolute", "[allow_files.\"payload.junk\"]\n\"/usr/lib/start.o\" = \"reason\"", "invalid_value"},
		{"parent", "[allow_files.\"payload.junk\"]\n\"usr/../start.o\" = \"reason\"", "invalid_value"},
		{"glob", "[allow_files.\"payload.junk\"]\n\"usr/[\" = \"reason\"", "invalid_value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "lint.pekit.toml")
			writeFile(t, file, tc.input)
			if _, err := loadLintFile(file); diagCode(err) != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}

func TestLintAllowFilesScopedAndUnused(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), `
[payload]
junk = true
[allow_files."payload.junk"]
"usr/lib/compiler/*/crtbegin.o" = "required startup object"
"usr/lib/unused.o" = "unused exception remains visible"
`)
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = 'mkdir -p "$PEKIT_OUT"; printf object > "$PEKIT_OUT/start"; printf object > "$PEKIT_OUT/stray"'
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
[package]
name = "example"
version = "1.0-1"
architecture = "x86_64"
[files]
":start" = "usr/lib/compiler/16/crtbegin.o"
":stray" = "usr/lib/stray.o"
`)
	if _, stderr, err := runIn(t, dir, "build", "--version", "1.0"); err != nil {
		t.Fatalf("build: %v\n%s", err, stderr)
	}
	events, err := lintEvents(t, dir, "lint", "--version", "1.0")
	if diagCode(err) != "lint_failed" || len(events["lint"]) != 1 || events["lint"][0].Path != "usr/lib/stray.o" {
		t.Fatalf("unrelated object must still fail: %v (%v)", err, events)
	}
	if len(events["lint_allowed"]) != 1 || len(events["lint_unused_allow"]) != 1 {
		t.Fatalf("expected one allowed and one unused exception: %v", events)
	}
}

func TestLintAllowFilesMerge(t *testing.T) {
	cfg := LintConfig{values: map[string]any{}, origin: map[string]string{}, Allow: map[string]lintAllow{}}
	for _, file := range []lintFileValues{
		{Path: "outer", AllowFiles: map[string]map[string]string{"payload.junk": {"usr/lib/start.o": "outer", "usr/lib/end.o": "retained"}}},
		{Path: "inner", AllowFiles: map[string]map[string]string{"payload.junk": {"usr/lib/start.o": "inner"}}},
	} {
		if err := cfg.merge(file); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.AllowFiles["payload.junk"]["usr/lib/start.o"].Reason != "inner" || cfg.AllowFiles["payload.junk"]["usr/lib/end.o"].Reason != "retained" {
		t.Fatalf("incorrect merge: %v", cfg.AllowFiles)
	}
}

func TestLintAllowFilesDoesNotLosePathsToFindingLimit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), `
[split]
devel.packages = "*-devel"
[allow_files."split.devel.packages"]
"usr/include/**" = "runtime-owned generated interface"
`)
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = 'mkdir -p "$PEKIT_OUT/include"; i=0; while [ "$i" -lt 12 ]; do printf header > "$PEKIT_OUT/include/$i.h"; i=$((i + 1)); done'
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
[package]
name = "example"
version = "1.0-1"
architecture = "noarch"
[files]
":include/**" = "usr/include"
`)
	if _, stderr, err := runIn(t, dir, "build", "--version", "1.0"); err != nil {
		t.Fatalf("build: %v\n%s", err, stderr)
	}
	events, err := lintEvents(t, dir, "lint", "--version", "1.0")
	if err != nil || len(events["lint"]) != 0 || len(events["lint_allowed"]) != 12 {
		t.Fatalf("all path-scoped findings should be allowed: %v (%v)", err, events)
	}
}
