package pekit

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// lintEvents runs pekit with --json in dir and returns the lint events by
// type, plus the command error.
func lintEvents(t *testing.T, dir string, args ...string) (map[string][]Event, error) {
	t.Helper()
	stdout, _, err := runIn(t, dir, append([]string{"--json"}, args...)...)
	byType := map[string][]Event{}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if jerr := json.Unmarshal([]byte(line), &e); jerr != nil {
			t.Fatalf("bad json event %q: %v", line, jerr)
		}
		byType[e.Type] = append(byType[e.Type], e)
	}
	return byType, err
}

func TestShebangLineIgnoresRustInnerAttribute(t *testing.T) {
	dir := t.TempDir()
	rustSource := filepath.Join(dir, "lib.rs")
	writeFile(t, rustSource, "#![no_std]\n")
	if line, ok := shebangLine(rustSource); ok {
		t.Fatalf("Rust inner attribute recognized as shebang: %q", line)
	}

	script := filepath.Join(dir, "script")
	writeFile(t, script, "#!/bin/sh\n")
	if line, ok := shebangLine(script); !ok || line != "/bin/sh" {
		t.Fatalf("valid shebang = %q, %v; want /bin/sh, true", line, ok)
	}
}

func lintRules(events []Event) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		out[e.Rule]++
	}
	return out
}

func TestLintConfigUnknownKeyAndBadValue(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[package]\nlicence = true\n")
	if _, err := loadLintFile(filepath.Join(dir, "lint.pekit.toml")); diagCode(err) != "unknown_key" {
		t.Fatalf("want unknown_key, got %v", err)
	}
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[package]\nname.style = \"camel\"\n")
	if _, err := loadLintFile(filepath.Join(dir, "lint.pekit.toml")); diagCode(err) != "invalid_value" {
		t.Fatalf("want invalid_value, got %v", err)
	}
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[allow]\n\"package.license\" = \"\"\n")
	if _, err := loadLintFile(filepath.Join(dir, "lint.pekit.toml")); diagCode(err) != "missing_reason" {
		t.Fatalf("want missing_reason, got %v", err)
	}
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[allow]\n\"payload.dirs.bin\" = \"not a rule\"\n")
	if _, err := loadLintFile(filepath.Join(dir, "lint.pekit.toml")); diagCode(err) != "unknown_key" {
		t.Fatalf("want unknown_key for a parameter in [allow], got %v", err)
	}
}

func TestLintConfigLayering(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), "include = [\"./*\"]\n")
	writeFile(t, filepath.Join(ws, "lint.pekit.toml"), "[package]\nlicense = true\ndescription = 80\n")
	member := filepath.Join(ws, "thing")
	writeFile(t, filepath.Join(member, "pekit.toml"), "[build.main]\ncommand = \"true\"\n")
	wsCfg, err := LoadWorkspace(filepath.Join(ws, "workspace.pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}

	// A nearer file may re-parameterise.
	writeFile(t, filepath.Join(member, "lint.pekit.toml"), "[package]\ndescription = 40\n")
	cfg, err := loadLintConfig(member, &wsCfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := cfg.Int("package.description"); n != 40 {
		t.Fatalf("nearer parameter did not win: %d", n)
	}
	if !cfg.Enabled("package.license") {
		t.Fatal("outer rule lost in merge")
	}

	// A nearer file may not switch a rule off.
	writeFile(t, filepath.Join(member, "lint.pekit.toml"), "[package]\nlicense = false\n")
	if _, err := loadLintConfig(member, &wsCfg, ""); diagCode(err) != "lint_rule_disabled" {
		t.Fatalf("want lint_rule_disabled, got %v", err)
	}

	// [allow] is the sanctioned way.
	writeFile(t, filepath.Join(member, "lint.pekit.toml"), "[allow]\n\"package.license\" = \"upstream ships no licence text yet\"\n")
	cfg, err = loadLintConfig(member, &wsCfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Allow["package.license"].Reason == "" {
		t.Fatal("allow entry not merged")
	}

	// A delegated source's file merges first, so the workspace wins.
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "lint.pekit.toml"), "[package]\ndescription = 200\n[source]\nlock = true\n")
	cfg, err = loadLintConfig(member, &wsCfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := cfg.Int("package.description"); n != 80 {
		t.Fatalf("source file overrode the workspace: %d", n)
	}
	if !cfg.Enabled("source.lock") {
		t.Fatal("source file's extra rule not merged")
	}
}

func TestSPDXExpressions(t *testing.T) {
	good := []string{
		"MIT",
		"GPL-2.0-or-later",
		"MIT AND (BSD-3-Clause OR GPL-2.0-only)",
		"GPL-2.0-or-later WITH Autoconf-exception-generic",
		"LicenseRef-Proprietary-Foo",
		"Apache-2.0 OR (LGPL-2.1+ AND MIT)",
	}
	for _, expr := range good {
		if err := validateSPDXExpression(expr); err != nil {
			t.Errorf("%q: unexpected error %v", expr, err)
		}
	}
	bad := []string{
		"",
		"MIT and BSD-3-Clause",
		"MIT AND",
		"(MIT",
		"MIT)",
		"MIT WITH",
		"GPL 2.0",
	}
	for _, expr := range bad {
		if err := validateSPDXExpression(expr); err == nil {
			t.Errorf("%q: expected an error", expr)
		}
	}
}

const lintAllStaticRules = `
[package]
name.style    = "reverse-dns"
license       = "spdx"
license_class = true
homepage      = "https"
description   = 80
dependencies  = "consistent"

[source]
reproducible          = true
discovery             = true
versions.floor        = true
versions.ceiling      = "none"
ref                   = "immutable"
url.scheme            = "https"
lock                  = true
signature.required    = true
signature.fingerprint = "full"
signature.keys        = true
patches.headers       = true

[build]
dependencies.providers = ["apt"]
test = true
`

func TestLintStaticFindings(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), "include = [\"./*\"]\n")
	writeFile(t, filepath.Join(ws, "lint.pekit.toml"), lintAllStaticRules)
	dir := filepath.Join(ws, "thing")
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.url]
url = "http://example.org/thing-{{version}}.tar.gz"
listing_url = "http://example.org/releases"
file_regex = 'thing-[0-9.]+\.tar\.gz'
versions = "< 2"

[source.url.signature]
key_files = ["keys/missing.asc"]
fingerprints = ["DEADBEEF"]

[build.main]
command = "true"
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
version = "{{version}}-1"
architecture = "x86_64"
description = "thing is a thing that does things and keeps doing them for as long as you let it run."
license = "MIT and BSD-3-Clause"
homepage = "http://example.org"

[dependencies]
thing = "*"
libfoo = "*"

[conflicts]
libfoo = "*"

[provides]
thing = "1"
`)
	events, err := lintEvents(t, dir, "lint")
	if diagCode(err) != "lint_failed" {
		t.Fatalf("want lint_failed, got %v", err)
	}
	got := lintRules(events["lint"])
	want := []string{
		"package.name.style", "package.license", "package.license_class", "package.homepage",
		"package.description", "package.dependencies",
		"source.versions.floor", "source.versions.ceiling", "source.url.scheme", "source.lock",
		"source.signature.fingerprint", "source.signature.keys",
		"build.dependencies.providers", "build.test",
	}
	for _, rule := range want {
		if got[rule] == 0 {
			t.Errorf("no finding for %s; got %v", rule, got)
		}
	}
	if got["source.signature.required"] != 0 {
		t.Errorf("signature.required fired although a signature block exists")
	}
	if got["source.url.scheme"] != 2 {
		t.Errorf("expected url and listing_url to be reported, got %d", got["source.url.scheme"])
	}
	if len(events["lint_summary"]) != 1 {
		t.Errorf("expected one summary event")
	}

	// A git source on a branch, discovering nothing and verifying nothing.
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source.git]
url = "git://example.org/thing.git"
ref = "main"

[build.main]
command = "true"
`)
	events, err = lintEvents(t, dir, "lint")
	if diagCode(err) != "lint_failed" {
		t.Fatalf("want lint_failed, got %v", err)
	}
	got = lintRules(events["lint"])
	for _, rule := range []string{"source.discovery", "source.ref", "source.url.scheme", "source.lock"} {
		if got[rule] == 0 {
			t.Errorf("no finding for %s on the git recipe; got %v", rule, got)
		}
	}
	if got["source.versions.floor"] != 0 || got["source.versions.ceiling"] != 0 || got["source.signature.required"] != 0 {
		t.Errorf("url-only rules fired on a git source: %v", got)
	}
}

func TestLintCleanRecipeAndAllow(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), "include = [\"./*\"]\n")
	writeFile(t, filepath.Join(ws, "lint.pekit.toml"), lintAllStaticRules)
	dir := filepath.Join(ws, "org.example.thing")
	writeFile(t, filepath.Join(dir, "keys", "release.asc"), "not really a key\n")
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
[source]
patches = "patches"

[source.url]
url = "https://example.org/thing-{{version}}.tar.gz"
listing_url = "https://example.org/releases"
file_regex = 'thing-[0-9.]+\.tar\.gz'
versions = ">= 1.0"

[source.url.signature]
key_files = ["keys/release.asc"]
fingerprints = ["4EF4AC63455FC9F4545D9B7DEF8FE99528B52FFD"]

[build.main]
command = "true"

[build.main.dependencies.apt]

[test.main]
command = "true"
`)
	writeFile(t, filepath.Join(dir, "patches", "series"), "0001-fix.patch\n")
	writeFile(t, filepath.Join(dir, "patches", "0001-fix.patch"), "From: Someone <s@example.org>\nSubject: fix the thing\n\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n")
	writeFile(t, filepath.Join(dir, "pekit.lock"), `schema = 1

[[source]]
  version = "1.0"
  url = "https://example.org/thing-1.0.tar.gz"
  sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
  signature_key = "4ef4ac63455fc9f4545d9b7def8fe99528b52ffd"
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "peipkg"

[package]
name = "org.example.thing"
version = "{{version}}-1"
architecture = "x86_64"
description = "A thing that does things"
license = "MIT AND (BSD-3-Clause OR GPL-2.0-only)"
license_class = "free"
homepage = "https://example.org"

[dependencies]
libfoo = "*"
`)
	events, err := lintEvents(t, dir, "lint")
	if err != nil {
		t.Fatalf("clean recipe failed lint: %v\n%v", err, events["lint"])
	}
	if len(events["lint"]) != 0 {
		t.Fatalf("unexpected findings: %v", events["lint"])
	}

	// Break one thing, then allow it with a reason.
	writeFile(t, filepath.Join(dir, "patches", "0001-fix.patch"), "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n")
	events, err = lintEvents(t, dir, "lint")
	if diagCode(err) != "lint_failed" || lintRules(events["lint"])["source.patches.headers"] != 1 {
		t.Fatalf("expected one patches.headers finding, got err=%v events=%v", err, events["lint"])
	}
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[allow]\n\"source.patches.headers\" = \"vendored from a tarball with no history\"\n\"build.test\" = \"never fires\"\n")
	events, err = lintEvents(t, dir, "lint")
	if err != nil {
		t.Fatalf("allowed finding still failed: %v", err)
	}
	if len(events["lint_allowed"]) != 1 || len(events["lint"]) != 0 {
		t.Fatalf("expected one allowed and no findings, got %v / %v", events["lint_allowed"], events["lint"])
	}
	if len(events["lint_unused_allow"]) != 1 {
		t.Fatalf("expected the unused build.test allow to be reported, got %v", events["lint_unused_allow"])
	}
}

func TestConstraintCeiling(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"*":               "",
		">= 1.5.7":        "",
		">= 1.5.7, > 1.0": "",
		">= 1.5.7, < 1.6": "<1.6",
		"<= 2":            "<=2",
		"= 1.2.3":         "=1.2.3",
		"^1.5":            "^1.5",
		">= 4.2, ~4.2":    "~4.2",
	}
	for raw, want := range cases {
		if got := constraintCeiling(raw); got != want {
			t.Errorf("constraintCeiling(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestLintNoConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), "[build.main]\ncommand = \"true\"\n")
	if _, err := lintEvents(t, dir, "lint"); diagCode(err) != "missing_lint_config" {
		t.Fatalf("want missing_lint_config, got %v", err)
	}
}

func TestLintRejectsSelectors(t *testing.T) {
	if _, err := ParseInvocation([]string{"lint", "main"}, "/tmp"); diagCode(err) != "invalid_selector" {
		t.Fatalf("want invalid_selector, got %v", err)
	}
	if _, err := ParseInvocation([]string{"lint", "--all"}, "/tmp"); diagCode(err) != "unsupported_flag" {
		t.Fatalf("want unsupported_flag, got %v", err)
	}
	if _, err := ParseInvocation([]string{"lint", "--no-build"}, "/tmp"); diagCode(err) != "unsupported_flag" {
		t.Fatalf("want unsupported_flag for --no-build, got %v", err)
	}
	if _, err := ParseInvocation([]string{"lint", "--version", "1.0", "--env", "x"}, "/tmp"); err != nil {
		t.Fatalf("version and env should be accepted: %v", err)
	}
}

// The payload rules against a real staged tree. The build compiles two
// deliberately bad objects with the host compiler: a non-PIE, unstripped
// program with an executable stack, no RELRO, a run path and no CET note,
// and a shared library with no SONAME.
func TestLintPayloadRules(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler on PATH")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), `
[package]
architecture = "consistent"

[payload]
junk = true
scripts = true
symlinks.dangling = "forbidden"
symlinks.absolute = "forbidden"
filenames = "portable"
special_files = "forbidden"
manpages.required = true
manpages.compression = "gzip"
pkgconfig = true
license_file = "usr/share/licenses/{{name}}/"

[split]
devel.packages = "*-devel"

[elf]
pie = true
stack = "non-exec"
relro = "full"
relr = true
cet = true
rpath = "forbidden"
textrel = "forbidden"
stripped = true
debuginfo = "{{name}}-debuginfo"
build_paths = "forbidden"
soname = true
`)
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[build.main]
command = '''
set -eu
mkdir -p "$PEKIT_OUT/usr/bin" "$PEKIT_OUT/usr/include" "$PEKIT_OUT/usr/lib" "$PEKIT_OUT/usr/share/man/man1"
printf 'int main(void){return 0;}\n' > "$PEKIT_OUT/t.c"
"`+cc+`" -no-pie -g -fcf-protection=none -Wl,-z,norelro -Wl,-z,execstack -Wl,-rpath,/opt/x -o "$PEKIT_OUT/usr/bin/tool" "$PEKIT_OUT/t.c"
printf 'int f(void){return 1;}\n' > "$PEKIT_OUT/l.c"
"`+cc+`" -shared -fPIC -o "$PEKIT_OUT/usr/lib/libl.so.1" "$PEKIT_OUT/l.c"
rm "$PEKIT_OUT/t.c" "$PEKIT_OUT/l.c"
printf '#!/nonexistent/sh\nif\n' > "$PEKIT_OUT/usr/bin/script"
chmod 0755 "$PEKIT_OUT/usr/bin/script"
printf '#!/bin/sh\necho ok\n' > "$PEKIT_OUT/usr/bin/plain"
chmod 0644 "$PEKIT_OUT/usr/bin/plain"
printf 'int x;\n' > "$PEKIT_OUT/usr/include/thing.h"
printf 'la\n' > "$PEKIT_OUT/usr/lib/libthing.la"
printf 'prefix=%s\n' "$PEKIT_OUT" > "$PEKIT_OUT/usr/lib/thing.pc"
printf 'page\n' > "$PEKIT_OUT/usr/share/man/man1/plain.1"
ln -s ../lib/nowhere.so.1 "$PEKIT_OUT/usr/bin/dangling"
ln -s /usr/bin/tool "$PEKIT_OUT/usr/bin/absolute"
printf 'x' > "$PEKIT_OUT/usr/bin/bad name"
'''
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `
format = "tar"

[package]
name = "thing"
version = "1.0-1"
architecture = "noarch"

[files]
":usr/**" = "usr"
`)
	if _, err := lintEvents(t, dir, "lint", "--version", "1.0"); diagCode(err) != "stage_missing" {
		t.Fatalf("want stage_missing before a build, got %v", err)
	}
	if _, stderr, err := runIn(t, dir, "build", "--version", "1.0"); err != nil {
		t.Fatalf("build: %v\n%s", err, stderr)
	}
	events, err := lintEvents(t, dir, "lint", "--version", "1.0")
	if diagCode(err) != "lint_failed" {
		t.Fatalf("want lint_failed, got %v", err)
	}
	got := lintRules(events["lint"])
	want := []string{
		"elf.pie", "elf.stripped", "elf.debuginfo", "elf.stack", "elf.relro", "elf.rpath", "elf.soname",
		"payload.junk", "payload.scripts", "payload.symlinks.dangling", "payload.symlinks.absolute",
		"payload.filenames", "payload.manpages.required", "payload.manpages.compression",
		"payload.pkgconfig", "payload.license_file", "package.architecture", "split.devel.packages",
	}
	if runtime.GOARCH == "amd64" {
		want = append(want, "elf.cet")
	}
	for _, rule := range want {
		if got[rule] == 0 {
			t.Errorf("no finding for %s; got %v", rule, got)
		}
	}
	// The library was built the ordinary way, so it must not trip the
	// program's findings.
	for _, e := range events["lint"] {
		if strings.HasSuffix(e.Path, "libl.so.1") && (e.Rule == "elf.pie" || e.Rule == "elf.stack" || e.Rule == "elf.rpath") {
			t.Errorf("library reported for %s: %v", e.Rule, e)
		}
	}
	// The script with a valid shebang and a man page must not be reported
	// for those two rules; the one with the broken shell must be.
	for _, e := range events["lint"] {
		if e.Rule == "payload.manpages.required" && strings.HasSuffix(e.Path, "/plain") {
			t.Errorf("plain has a man page but was reported: %v", e)
		}
		if e.Rule == "payload.scripts" && strings.HasSuffix(e.Path, "/plain") && !strings.Contains(e.Message, "not executable") {
			t.Errorf("plain reported for something other than its mode: %v", e)
		}
	}
	if got["payload.scripts"] < 3 {
		t.Errorf("expected findings for the bad interpreter, the syntax error and the mode; got %d", got["payload.scripts"])
	}
	if got["elf.textrel"] != 0 {
		t.Errorf("unexpected textrel finding: %v", got)
	}
}
