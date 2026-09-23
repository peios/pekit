package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gateFixture is a committed single-recipe catalogue with two signed peipkg
// packages, a lint policy, a publish target and keys for both signatures.
func gateFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	keyDir := t.TempDir()
	key, _ := writeSeedKey(t, keyDir)
	writeFile(t, filepath.Join(dir, ".gitignore"), "out\nrepo\n.pekit-job.lock\n")
	writeFile(t, filepath.Join(dir, "pekit.toml"), "out_dir = 'out'\n")
	writeFile(t, filepath.Join(dir, "lint.pekit.toml"), "[package]\nlicense = true\n[payload]\njunk = true\n")
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, name+".txt"), "hello "+name)
		writeFile(t, filepath.Join(dir, name+".package.pekit.toml"), `format = 'peipkg'
[package]
name = 'fixture-`+name+`'
version = '1.0.0-1'
architecture = 'noarch'
description = 'gate fixture'
license = 'MIT'
[files]
'@recipe:`+name+`.txt' = 'usr/share/`+name+`/payload.txt'
[publish.peipkg]
path = 'repo'
name = 'gate-test'
signing_key = 'keyring:signing.repository_key'
`)
	}
	runTestCmd(t, dir, "git", "init", "-q")
	gitCommitAll(t, dir)
	return dir, []string{"--keyring.signing.package_key=" + key, "--keyring.signing.repository_key=" + key}
}

func gitCommitAll(t *testing.T, dir string) {
	t.Helper()
	runTestCmd(t, dir, "git", "add", "-A")
	runTestCmd(t, dir, "git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
}

func runGateCmd(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	chdir(t, dir)
	var out bytes.Buffer
	app := &App{Stdout: &out, Stderr: &out, Now: func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }}
	err := app.Run(args)
	return out.String(), err
}

func editFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	writeFile(t, path, strings.Replace(string(data), old, new, 1))
}

func TestPackageLintGateStopsFindingsBeforePublication(t *testing.T) {
	dir, keys := gateFixture(t)
	editFile(t, filepath.Join(dir, "a.package.pekit.toml"), "usr/share/a/payload.txt", "usr/share/a/unwanted.la")
	out, err := runGateCmd(t, dir, append([]string{"publish", "--all"}, keys...)...)
	if err == nil || !strings.Contains(err.Error(), "lint_failed") {
		t.Fatalf("want lint_failed, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "payload.junk") {
		t.Fatalf("finding not reported:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "repo")); !os.IsNotExist(err) {
		t.Fatalf("a lint failure reached the repository: %v", err)
	}
	if out, err := runGateCmd(t, dir, append([]string{"package", "--all", "--no-gates"}, keys...)...); err != nil {
		t.Fatalf("--no-gates did not skip lint: %v\n%s", err, out)
	}
}

func TestPackageLintGatePassesCleanPackages(t *testing.T) {
	dir, keys := gateFixture(t)
	// Claims name logical paths inside a package; the gate reads them from the
	// finished archives, across both packages.
	editFile(t, filepath.Join(dir, "a.package.pekit.toml"), "[files]", `[provides]
fixture-role = '1'
[claims.provides.fixture-role.command]
target = '/usr/share/a/payload.txt'
path = '/usr/bin/fixture-role'
[files]`)
	editFile(t, filepath.Join(dir, "b.package.pekit.toml"), "[files]", `[dependencies]
fixture-role = '*'
[claims.dependencies.fixture-role.command]
path = '/usr/bin/fixture-consumer'
[files]`)
	out, err := runGateCmd(t, dir, append([]string{"publish", "--all"}, keys...)...)
	if err != nil {
		t.Fatalf("clean packages failed the gate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "0 finding(s)") {
		t.Fatalf("gate did not run:\n%s", out)
	}
	if got := len(readPublishedIndex(t, filepath.Join(dir, "repo/index/active.json")).Packages); got != 2 {
		t.Fatalf("published %d packages", got)
	}
}

func TestEnvLintAllowExemptsTheEnvironmentsRule(t *testing.T) {
	dir, keys := gateFixture(t)
	editFile(t, filepath.Join(dir, "a.package.pekit.toml"), "usr/share/a/payload.txt", "usr/share/a/unwanted.la")
	writeFile(t, filepath.Join(dir, "reference.env.pekit.toml"), `[env]
FIXTURE = '1'
[lint.allow]
'payload.junk' = 'the reference toolchain leaves libtool archives'
`)
	out, err := runGateCmd(t, dir, append([]string{"package", "--all", "--env", "reference"}, keys...)...)
	if err != nil {
		t.Fatalf("environment exemption not applied: %v\n%s", err, out)
	}
	if !strings.Contains(out, "allowed: the reference toolchain") {
		t.Fatalf("exemption not reported:\n%s", out)
	}
	if _, err := runGateCmd(t, dir, append([]string{"package", "--all"}, keys...)...); err == nil {
		t.Fatal("exemption leaked outside its environment")
	}
}

func TestEnvLintAllowRejectsUnknownRuleOrEmptyReason(t *testing.T) {
	for name, body := range map[string]string{
		"unknown": "[env]\nX = '1'\n[lint.allow]\n'no.such.rule' = 'reason'\n",
		"empty":   "[env]\nX = '1'\n[lint.allow]\n'payload.junk' = ' '\n",
		"key":     "[env]\nX = '1'\n[lint.deny]\n'payload.junk' = 'reason'\n",
	} {
		path := filepath.Join(t.TempDir(), "x.env.pekit.toml")
		writeFile(t, path, body)
		if _, err := LoadEnvFile(path, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStrictRejectsBypassFlags(t *testing.T) {
	for _, flag := range []string{"--no-gates", "--no-build", "--no-verify", "--allow-unsigned", "--allow-unanchored", "--local", "--prefer-local"} {
		if _, err := ParseInvocation([]string{"publish", "--strict", flag}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "strict_bypass") {
			t.Errorf("publish --strict %s: %v", flag, err)
		}
	}
	for _, cmd := range []string{"build", "test", "package", "publish"} {
		if _, err := ParseInvocation([]string{cmd, "--strict"}, t.TempDir()); err != nil {
			t.Errorf("%s --strict: %v", cmd, err)
		}
	}
	if _, err := ParseInvocation([]string{"lint", "--strict"}, t.TempDir()); err == nil {
		t.Error("lint accepted --strict")
	}
	if _, err := ParseInvocation([]string{"release"}, t.TempDir()); err == nil {
		t.Error("release is still a command")
	}
}

func TestStrictRequiresCommittedCatalogue(t *testing.T) {
	dir, keys := gateFixture(t)
	args := append([]string{"package", "--all", "--strict"}, keys...)
	if out, err := runGateCmd(t, dir, args...); err != nil {
		t.Fatalf("clean catalogue refused: %v\n%s", err, out)
	}
	// An unattended lock update is not a review blocker.
	writeFile(t, filepath.Join(dir, "pekit.lock"), "schema = 1\n")
	if out, err := runGateCmd(t, dir, args...); err != nil {
		t.Fatalf("lock-only change refused: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "unreviewed")
	if _, err := runGateCmd(t, dir, args...); err == nil || !strings.Contains(err.Error(), "strict_dirty") {
		t.Fatalf("dirty catalogue accepted: %v", err)
	}
}

func TestStrictPublishChecksInstallClosures(t *testing.T) {
	dir, keys := gateFixture(t)
	editFile(t, filepath.Join(dir, "b.package.pekit.toml"), "[files]", "[dependencies]\nfixture-missing = '*'\n[files]")
	gitCommitAll(t, dir)
	out, err := runGateCmd(t, dir, append([]string{"publish", "--all", "--strict"}, keys...)...)
	if err == nil || !strings.Contains(err.Error(), "qualification") {
		t.Fatalf("strict publish accepted an unresolvable closure: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "repo/index/active.json")); err == nil {
		if n := len(readPublishedIndex(t, filepath.Join(dir, "repo/index/active.json")).Packages); n != 0 {
			t.Fatalf("refused publication still indexed %d packages", n)
		}
	}
	// Ordinary publication stays incremental: a bootstrap publishes packages
	// ahead of the dependencies they need.
	if out, err := runGateCmd(t, dir, append([]string{"publish", "--all"}, keys...)...); err != nil {
		t.Fatalf("ordinary publish refused an incomplete repository: %v\n%s", err, out)
	}
}

func TestClaimPayloadPathRejectsTraversal(t *testing.T) {
	for _, value := range []string{"", "/", "usr/bin/sh", "../usr/bin/sh", "/../usr/bin/sh", "/usr/../bin/sh", "//usr/bin/sh", "/usr/./bin/sh", "/usr/bin/sh/", "/usr/bin/\x00sh"} {
		if _, err := claimPayloadPath(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	for value, want := range map[string]string{"/usr/bin/sh": "usr/bin/sh", "/init": "init", "/run/service/socket": "run/service/socket"} {
		if got, err := claimPayloadPath(value); err != nil || got != want {
			t.Errorf("%q: %q, %v", value, got, err)
		}
	}
}
