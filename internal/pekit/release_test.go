package pekit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peios/peipkg/repopub"
)

func releaseFixture(t *testing.T, checks string) (string, []string) {
	t.Helper()
	ws := t.TempDir()
	keydir := t.TempDir()
	key, _ := writeSeedKey(t, keydir)
	writeFile(t, filepath.Join(ws, ".gitignore"), "out\n.pekit\n.pekit-job.lock\npublic\n")
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), `include=['a','b']
[isolation]
enabled=true
[release]
path='public'
name='test-release'
signing_key='keyring:signing.repository_key'
environments=['reference','native']
`+checks)
	for _, env := range []string{"reference", "native"} {
		writeFile(t, filepath.Join(ws, env+".env.pekit.toml"), "[sandbox]\ncommand='false'\n")
	}
	writeFile(t, filepath.Join(ws, "lint.pekit.toml"), "[package]\nlicense=true\n[payload]\njunk=true\n")
	for _, name := range []string{"a", "b"} {
		root := filepath.Join(ws, name)
		writeFile(t, filepath.Join(root, "pekit.toml"), "out_dir='out'\n")
		writeFile(t, filepath.Join(root, "payload.txt"), "hello "+name)
		writeFile(t, filepath.Join(root, "package.pekit.toml"), `format='peipkg'
[package]
name='fixture-`+name+`'
version='1.0.0-1'
architecture='noarch'
description='release fixture'
license='MIT'
[files]
'@recipe:payload.txt'='usr/share/`+name+`/payload.txt'
`)
	}
	runTestCmd(t, ws, "git", "init", "-q")
	runTestCmd(t, ws, "git", "add", ".")
	releaseCommit(t, ws)
	return ws, []string{"--keyring.signing.package_key=" + key, "--keyring.signing.repository_key=" + key}
}
func releaseCommit(t *testing.T, ws string) {
	t.Helper()
	runTestCmd(t, ws, "git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qam", "fixture")
}
func runRelease(t *testing.T, ws string, args []string) error {
	t.Helper()
	chdir(t, ws)
	var out, errs bytes.Buffer
	app := &App{Stdout: &out, Stderr: &errs}
	err := app.Run(args)
	if err != nil {
		t.Logf("%v\n%s\n%s", err, out.String(), errs.String())
	}
	return err
}
func TestReleaseWorkspaceBatchesFixedArtifacts(t *testing.T) {
	ws, args := releaseFixture(t, "[release.checks]\ninspect='test -f \"$PEKIT_RELEASE_REPOSITORY/index/active.json\"'\n")
	if err := runRelease(t, ws, append([]string{"workspace", "--jobs", "2", "release", "--all"}, args...)); err != nil {
		t.Fatal(err)
	}
	index := readPublishedIndex(t, filepath.Join(ws, "public/index/active.json"))
	if index.IndexVersion != 2 || len(index.Packages) != 2 {
		t.Fatalf("not one batch: %+v", index)
	}
	dirs, err := filepath.Glob(filepath.Join(ws, ".pekit/releases/candidate-*"))
	if err != nil || len(dirs) != 1 {
		t.Fatal(dirs, err)
	}
	var record struct {
		Qualified bool
		Artifacts []releaseArtifact
		Evidence  map[string]string
	}
	data, err := os.ReadFile(filepath.Join(dirs[0], "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if !record.Qualified || len(record.Artifacts) != 2 || len(record.Evidence) == 0 {
		t.Fatal("incomplete evidence")
	}
	for _, a := range record.Artifacts {
		if !strings.Contains(a.Path, "/native/") {
			t.Fatal("reference artifact promoted")
		}
		got, e := releaseHash(a.Path)
		if e != nil || got != a.SHA256 {
			t.Fatal("artifact identity")
		}
	}
	// The protected repository refuses the old publisher path even with a key.
	if _, err = repopub.Publish(filepath.Join(ws, "public"), repopub.PublishOptions{}); err == nil || !strings.Contains(err.Error(), "qualification_required") {
		t.Fatalf("ordinary publish bypass: %v", err)
	}
	// A subsequent release checks a protected base and advances one index.
	for _, name := range []string{"a", "b"} {
		p := filepath.Join(ws, name, "package.pekit.toml")
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		writeFile(t, p, strings.ReplaceAll(string(b), "1.0.0-1", "1.0.0-2"))
	}
	releaseCommit(t, ws)
	if err := runRelease(t, ws, append([]string{"workspace", "release", "--all"}, args...)); err != nil {
		t.Fatal(err)
	}
	if got := readPublishedIndex(t, filepath.Join(ws, "public/index/active.json")).IndexVersion; got != 3 {
		t.Fatalf("index = %d", got)
	}
}
func TestReleaseFailureDoesNotCreateProductionRepository(t *testing.T) {
	for _, tc := range []struct {
		name, checks string
		mutate       func(*testing.T, string)
		want         string
	}{
		{name: "check", checks: "[release.checks]\nfail='exit 3'\n", want: "release check fail failed"},
		{name: "tamper", checks: "[release.checks]\ntamper='printf changed >> \"$PEKIT_RELEASE_DIR/selection.json\"'\n", want: "release_stale"},
		{name: "lint", mutate: func(t *testing.T, ws string) {
			p := filepath.Join(ws, "a/package.pekit.toml")
			b, _ := os.ReadFile(p)
			writeFile(t, p, strings.ReplaceAll(string(b), "usr/share/a/payload.txt", "usr/share/a/unwanted.la"))
			releaseCommit(t, ws)
		}, want: "workspace_failed"},
		{name: "dirty", mutate: func(t *testing.T, ws string) { writeFile(t, filepath.Join(ws, "a/payload.txt"), "unreviewed") }, want: "workspace_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, args := releaseFixture(t, tc.checks)
			if tc.mutate != nil {
				tc.mutate(t, ws)
			}
			err := runRelease(t, ws, append([]string{"workspace", "release", "--all"}, args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s got %v", tc.want, err)
			}
			if _, err = os.Stat(filepath.Join(ws, "public")); !os.IsNotExist(err) {
				t.Fatal("failed qualification touched production")
			}
		})
	}
}
func TestReleaseRejectsBypassFlags(t *testing.T) {
	for _, flag := range []string{"--no-gates", "--no-build", "--no-verify", "--allow-unsigned", "--allow-unanchored", "--local"} {
		if _, err := ParseInvocation([]string{"release", flag}, t.TempDir()); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}

func TestReleaseRealIsolatedBuildAndGates(t *testing.T) {
	if os.Getenv("PEKIT_TEST_SANDBOX") != "1" {
		t.Skip("set PEKIT_TEST_SANDBOX=1 for real namespace builds")
	}
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			ws, args := releaseFixture(t, "")
			image := sandboxFixtureRoot(t)
			prepare := fmt.Sprintf("cp -a %s \"$PEKIT_SANDBOX_ROOT\"\nrecord=\"$PEKIT_JOB_STATE/dependencies/$PEKIT_COMMAND-$PEKIT_TARGET\"\nmkdir -p \"$record\"\nprintf 'fixed-fixture-root' > \"$record/inventory\"", shellQuote(image+"/."))
			for _, env := range []string{"reference", "native"} {
				writeFile(t, filepath.Join(ws, env+".env.pekit.toml"), "[sandbox]\ncommand="+fmt.Sprintf("%q", prepare)+"\n")
			}
			gate := "test \"$(cat \"$PEKIT_MAIN_OUT/payload.txt\")\" = hello"
			if failure {
				gate = "exit 7"
			}
			build := `root=${PEKIT_OUT%/*}; root=${root%/*}
test "$PEKIT_SOURCE_ROOT" = "$root/source"
test "$PEKIT_LITERAL_ROOT" = "$PEKIT_SOURCE_ROOT"
test ! -L "$PEKIT_SOURCE_ROOT"
test ! -e "$PEKIT_SOURCE_ROOT/worker-mutation"
printf private > "$PEKIT_SOURCE_ROOT/worker-mutation"
printf hello > "$PEKIT_OUT/payload.txt"`
			writeFile(t, filepath.Join(ws, "a/pekit.toml"), "[build.main]\ncommand="+fmt.Sprintf("%q", build)+"\n[test.check]\ngate=true\nneeds=['main']\ncommand="+fmt.Sprintf("%q", gate)+"\n")
			p := filepath.Join(ws, "a/package.pekit.toml")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, p, strings.ReplaceAll(string(data), "@recipe:payload.txt", "main:payload.txt"))
			releaseCommit(t, ws)
			err = runRelease(t, filepath.Join(ws, "a"), append([]string{"release", "--all"}, args...))
			if failure {
				if err == nil {
					t.Fatal("failed gate published")
				}
				if _, e := os.Stat(filepath.Join(ws, "public")); !os.IsNotExist(e) {
					t.Fatal("failed gate created repository")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := filepath.Glob(filepath.Join(ws, "a/out/.pekit-release-sources/candidate-*/*"))
			if err != nil || len(frozen) != 1 {
				t.Fatalf("missing frozen source: %v, %v", frozen, err)
			}
			if _, err := os.Stat(filepath.Join(frozen[0], "worker-mutation")); !os.IsNotExist(err) {
				t.Fatalf("worker changed frozen source: %v", err)
			}
			paths, err := filepath.Glob(filepath.Join(ws, ".pekit/releases/candidate-*/*/*/qualification.json"))
			if err != nil || len(paths) != 2 {
				t.Fatal(paths, err)
			}
			for _, p := range paths {
				var r releaseReceipt
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(data, &r); err != nil {
					t.Fatal(err)
				}
				if len(r.Gates) != 1 || len(r.EnvironmentFiles) < 2 {
					t.Fatalf("missing build/test root identities: %+v", r)
				}
			}
		})
	}
}

func TestReleaseLatestDoesNotChangeCandidateBetweenChecks(t *testing.T) {
	upstream := t.TempDir()
	writeFile(t, filepath.Join(upstream, "payload.txt"), "upstream-one")
	runTestCmd(t, upstream, "git", "init", "-q")
	runTestCmd(t, upstream, "git", "add", ".")
	releaseCommit(t, upstream)
	runTestCmd(t, upstream, "git", "tag", "v1.0.0")
	checks := "[release.checks]\nnew-upstream=" + fmt.Sprintf("%q", "git -C "+shellQuote(upstream)+" tag -f v2.0.0") + "\n"
	ws, args := releaseFixture(t, checks)
	writeFile(t, filepath.Join(ws, "a/pekit.toml"), "[source.git]\nurl="+fmt.Sprintf("%q", upstream)+"\nref='v{{version}}'\n")
	p := filepath.Join(ws, "a/package.pekit.toml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, strings.ReplaceAll(strings.ReplaceAll(string(b), "@recipe:payload.txt", "@source:payload.txt"), "1.0.0-1", "{{version}}-1"))
	releaseCommit(t, ws)
	if err = runRelease(t, filepath.Join(ws, "a"), append([]string{"release", "--all", "--latest"}, args...)); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(ws, ".pekit/releases/candidate-*/*/*/qualification.json"))
	if err != nil || len(paths) != 2 {
		t.Fatal(paths, err)
	}
	for _, p := range paths {
		var r releaseReceipt
		data, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(data, &r); e != nil {
			t.Fatal(e)
		}
		if r.Version != "1.0.0" {
			t.Fatalf("candidate changed during release: %s", r.Version)
		}
	}
	// Upstream 2.0.0 now exists, but both the binary and source artifact from
	// the completed attempt must still identify 1.0.0.
	index := readPublishedIndex(t, filepath.Join(ws, "public/index/active.json"))
	if len(index.Packages) != 2 {
		t.Fatalf("missing corresponding source: %+v", index)
	}
	data, err := os.ReadFile(filepath.Join(ws, "public/index/active.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "2.0.0") {
		t.Fatal("new upstream slipped into promotion")
	}
	// The next unattended attempt may discover 2.0.0 without a human committing
	// the lock generated by the successful 1.0.0 attempt.
	if err = runRelease(t, filepath.Join(ws, "a"), append([]string{"release", "--all", "--latest"}, args...)); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(ws, "public/index/active.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "2.0.0-1") {
		t.Fatal("next unattended release did not advance")
	}

}

func TestReleaseAcceptsLogicalClaimPaths(t *testing.T) {
	ws, args := releaseFixture(t, "")
	p := filepath.Join(ws, "a/package.pekit.toml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, string(data)+`
[provides]
fixture-role='1'
[claims.provides.fixture-role.command]
target='/usr/share/a/payload.txt'
path='/usr/bin/fixture-role'
`)
	p = filepath.Join(ws, "b/package.pekit.toml")
	data, err = os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, string(data)+`
[dependencies]
fixture-role='*'
[claims.dependencies.fixture-role.command]
path='/usr/bin/fixture-consumer'
`)
	releaseCommit(t, ws)
	if err := runRelease(t, ws, append([]string{"workspace", "release", "--all"}, args...)); err != nil {
		t.Fatal(err)
	}
	if got := len(readPublishedIndex(t, filepath.Join(ws, "public/index/active.json")).Packages); got != 2 {
		t.Fatalf("published %d packages", got)
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
