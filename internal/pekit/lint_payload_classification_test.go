package pekit

import (
	"path/filepath"
	"testing"
)

func TestLintArchitectureSourceAndTripletPaths(t *testing.T) {
	for _, tc := range []struct {
		name, dest, arch string
		wantFinding      bool
	}{
		{"debugsource", "usr/src/debug/example/lib.rs", "noarch", false},
		{"triplet-devel", "usr/lib/x86_64-linux-peios/pkgconfig/example.pc", "x86_64", false},
		{"triplet-noarch", "usr/lib/x86_64-linux-peios/pkgconfig/example.pc", "noarch", true},
		{"ordinary-data", "usr/share/example/data.txt", "noarch", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "lint.pekit.toml"), `
[package]
architecture = "consistent"
[payload]
dirs.lib = ["usr/lib", "usr/lib/*-linux-*"]
`)
			writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = 'mkdir -p "$PEKIT_OUT"; printf source > "$PEKIT_OUT/text"'
`)
			writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
[package]
name = "example"
version = "1.0-1"
architecture = "`+tc.arch+`"
[files]
":text" = "`+tc.dest+`"
`)
			if _, stderr, err := runIn(t, dir, "build", "--version", "1.0"); err != nil {
				t.Fatalf("build: %v\n%s", err, stderr)
			}
			events, err := lintEvents(t, dir, "lint", "--version", "1.0")
			if err != nil && diagCode(err) != "lint_failed" {
				t.Fatal(err)
			}
			if got := lintRules(events["lint"])["package.architecture"] > 0; got != tc.wantFinding {
				t.Fatalf("architecture finding = %v, want %v: %v", got, tc.wantFinding, events["lint"])
			}
		})
	}
}

func TestLintDevelSplitIgnoresEmptyDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), `
[split]
devel.packages = "*-devel"
`)
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = 'mkdir -p "$PEKIT_OUT/usr/include/x86_64-linux-peios"'
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
[package]
name = "filesystem"
version = "1.0-1"
architecture = "noarch"
[files]
":usr/**" = "usr"
`)
	if _, stderr, err := runIn(t, dir, "build", "--version", "1.0"); err != nil {
		t.Fatalf("build: %v\n%s", err, stderr)
	}
	if events, err := lintEvents(t, dir, "lint", "--version", "1.0"); err != nil {
		t.Fatalf("empty include directories should not require a devel split: %v (%v)", err, events["lint"])
	}
}

func TestLintArchitectureAccountsForSiblingDependency(t *testing.T) {
	for _, tc := range []struct{ name, arch, dest string }{
		{"constrained-source", "x86_64", "usr/src/debug/example/lib.rs"},
		{"portable-source", "noarch", "usr/src/debug/example/lib.rs"},
		{"portable-script", "noarch", "usr/bin/example-helper"},
		{"portable-data", "noarch", "usr/share/example/data.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[package]\narchitecture = \"consistent\"\n")
			writeFile(t, filepath.Join(dir, "pekit.toml"), `
[build.main]
command = 'mkdir -p "$PEKIT_OUT"; printf source > "$PEKIT_OUT/text"'
`)
			writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"
[package]
version = "1.0-1"
`)
			writeFile(t, filepath.Join(dir, "runtime.package.pekit.toml"), `
[package]
name = "example-runtime"
architecture = "x86_64"
[files]
":text" = "usr/lib/x86_64-linux-example/config.txt"
`)
			writeFile(t, filepath.Join(dir, "source.package.pekit.toml"), `
[package]
name = "example-debugsource"
architecture = "`+tc.arch+`"
[dependencies]
example-runtime = "1.0-1"
[files]
":text" = "`+tc.dest+`"
`)
			if _, stderr, err := runIn(t, dir, "build", "--version", "1.0"); err != nil {
				t.Fatalf("build: %v\n%s", err, stderr)
			}
			events, err := lintEvents(t, dir, "lint", "--version", "1.0")
			if err != nil {
				t.Fatalf("portable payload with native sibling rejected for %s: %v (%v)", tc.arch, err, events["lint"])
			}
		})
	}
}
