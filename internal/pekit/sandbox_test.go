package pekit

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkerKeyringAccess(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test.keyring.pekit.toml"), `
secret = "private"
public = { value = "public-value", access = "public" }
token = { value = "credential", access = "acquisition" }
`)
	inv := Invocation{Cwd: root, Keyrings: []string{"test"}}
	values, err := resolveKeyrings(inv, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		target TargetConfig
		want   string
		fail   bool
	}{
		{"default", TargetConfig{Kind: CommandBuild, Name: "main"}, "", false},
		{"public", TargetConfig{Kind: CommandBuild, Name: "main", KeyringInputs: []string{"public"}}, "public-value", false},
		{"secret", TargetConfig{Kind: CommandBuild, Name: "main", KeyringInputs: []string{"secret"}}, "", true},
		{"compile credential", TargetConfig{Kind: CommandBuild, Name: "main", KeyringInputs: []string{"token"}}, "", true},
		{"vendor credential", TargetConfig{Kind: CommandBuild, Name: "vendor", KeyringInputs: []string{"token"}}, "credential", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workerKeyrings(inv, root, nil, tc.target, values)
			if (err != nil) != tc.fail {
				t.Fatalf("values=%v error=%v", got, err)
			}
			if tc.fail {
				return
			}
			if tc.want == "" && len(got) != 0 {
				t.Fatal(got)
			}
			if tc.want != "" {
				for _, v := range got {
					if v != tc.want {
						t.Fatal(got)
					}
				}
			}
		})
	}
	inv.KeyringValues = map[string]string{"public": "private-override"}
	if _, err := workerKeyrings(inv, root, nil, TargetConfig{KeyringInputs: []string{"public"}}, values); err == nil {
		t.Fatal("CLI override retained a public grant")
	}
	t.Setenv("PEKIT_KEYRING_INHERITED", "private")
	for _, entry := range envSlice(nil) {
		if strings.HasPrefix(entry, "PEKIT_KEYRING_") {
			t.Fatal("inherited keyring export", entry)
		}
	}
}

func TestSandboxProbe(t *testing.T) {
	if os.Getenv("PEKIT_SANDBOX_PROBE") != "1" {
		return
	}
	for _, path := range strings.Split(os.Getenv("FORBIDDEN_PATHS"), "|") {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("host path visible: %s (%v)", path, err)
		}
	}
	if os.Getenv("HOST_SECRET") != "" || os.Getenv("PEKIT_KEYRING_SECRET") != "" {
		t.Fatal("host environment leaked")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 {
			t.Fatalf("network interface escaped isolation: %s", i.Name)
		}
	}
}

// A fixture root holds only explicitly copied executables and their dynamic
// loader dependencies, never a bind of the host root or home directory.
func sandboxFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyBinary := func(src, dest string) {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, dest), string(data))
		if err := os.Chmod(filepath.Join(root, dest), 0755); err != nil {
			t.Fatal(err)
		}
	}
	binaries := map[string]string{"/bin/sh": "bin/sh", "/bin/sleep": "bin/sleep", "/bin/cat": "bin/cat", "/bin/ln": "bin/ln"}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaries[self] = "usr/bin/probe"
	for src, dest := range binaries {
		copyBinary(src, dest)
		output, _ := exec.Command("ldd", src).Output()
		for _, word := range strings.Fields(string(output)) {
			if strings.HasPrefix(word, "/") {
				copyBinary(word, strings.TrimPrefix(word, "/"))
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../bin/sh", filepath.Join(root, "usr/bin/sh")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSandboxJobIsolation(t *testing.T) {
	if os.Getenv("PEKIT_TEST_SANDBOX") != "1" {
		t.Skip("set PEKIT_TEST_SANDBOX=1 on a host with unprivileged namespaces")
	}
	root := sandboxFixtureRoot(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	writeFile(t, secret, "private")
	ws := filepath.Join(dir, "workspace")
	recipe := filepath.Join(ws, "package")
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), `include=["package"]
[isolation]
enabled=true
inputs=["helpers"]
`)
	writeFile(t, filepath.Join(ws, "helpers/tool"), "helper")
	writeFile(t, filepath.Join(ws, "sibling/secret"), "sibling")
	writeFile(t, filepath.Join(ws, "_peipkgRepo_/secret"), "repository")
	writeFile(t, filepath.Join(recipe, "private.keyring.pekit.toml"), "secret='private'")
	writeFile(t, filepath.Join(ws, "test.env.pekit.toml"), "dependency_provider='apt'\n[sandbox]\ncommand="+fmt.Sprintf("%q", "cp -a "+shellQuote(root+"/.")+" \"$PEKIT_SANDBOX_ROOT\"")+"\n")
	forbidden := strings.Join([]string{secret, filepath.Join(ws, "sibling/secret"), filepath.Join(ws, "_peipkgRepo_/secret"), filepath.Join(recipe, "private.keyring.pekit.toml"), filepath.Join(os.Getenv("HOME"), ".ssh")}, "|")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), fmt.Sprintf(`
out_dir="out"
[env]
PEKIT_UNUSED=""
[build.main]
command='''
export PEKIT_SANDBOX_PROBE=1 FORBIDDEN_PATHS=%s
/usr/bin/probe -test.run='^TestSandboxProbe$'
test "$(cat "$PEKIT_WORKSPACE_ROOT/helpers/tool")" = helper
test -r "$PEKIT_DEPENDENCIES_FILE"
if printf corrupt > "$PEKIT_DEPENDENCIES_FILE" 2>/dev/null; then exit 1; fi
printf generated > generated
printf retained > "$PEKIT_OUT/state"
(sleep 1; printf escaped > "$PEKIT_OUT/background") >/dev/null 2>&1 &
'''
[test.main]
needs=["main"]
command='''
test "$(cat generated)" = generated
case "$(cat "$PEKIT_MAIN_OUT/state")" in retained*) ;; *) exit 1;; esac
printf checked >> "$PEKIT_MAIN_OUT/state"
'''
[gen.source]
command='printf generated > generated.txt'
verify_command='printf private > verification-only'
verify_on_build=[]
verify_on_test=[]

`, shellQuote(forbidden)))
	// A source-supplied wrap must never execute on the coordinator.
	data, err := os.ReadFile(filepath.Join(recipe, "pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("[env]\nPEKIT_UNUSED=\"\""), []byte("[wrap]\ncommand='echo compromised > "+secret+"; {{command}}'"), 1)
	writeFile(t, filepath.Join(recipe, "pekit.toml"), string(data))
	t.Setenv("HOST_SECRET", "secret")
	t.Setenv("PEKIT_KEYRING_SECRET", "secret")
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"test", "--env=test"}); err != nil {
		t.Fatalf("%v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(recipe, "generated")); !os.IsNotExist(err) {
		t.Fatal("source checkout was modified")
	}
	if _, err := os.Stat(filepath.Join(recipe, "out/build/main/background")); !os.IsNotExist(err) {
		t.Fatal("background writer survived")
	}
	got, _ := os.ReadFile(secret)
	if string(got) != "private" {
		t.Fatal("source wrapper escaped", string(got))
	}
	if err := app.Run([]string{"test", "--env=test", "--no-build"}); err != nil {
		t.Fatalf("retained source: %v\n%s", err, stderr.String())
	}
	if err := app.Run([]string{"gen", "source", "--env=test"}); err != nil {
		t.Fatalf("gen: %v\n%s", err, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(recipe, "generated.txt")); err != nil || string(got) != "generated" {
		t.Fatalf("generated source was not applied: %q %v", got, err)
	}
	if err := app.Run([]string{"verify", "source", "--env=test"}); err != nil {
		t.Fatalf("verify: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(recipe, "verification-only")); !os.IsNotExist(err) {
		t.Fatal("verify modified committed source")
	}
	if err := app.Run([]string{"build", "--env=none"}); err == nil {
		t.Fatal("isolation was bypassed with env none")
	}
}

func TestSourceSnapshotDoesNotBindSymlinkRoot(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	link := filepath.Join(dir, "link")
	copy := filepath.Join(dir, "copy")
	writeFile(t, filepath.Join(source, "file"), "original")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := snapshotTree(link, copy, filepath.Join(source, "out"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(copy)
	if err != nil || !st.IsDir() {
		t.Fatal("snapshot retained source-root symlink", err)
	}
	writeFile(t, filepath.Join(copy, "file"), "modified")
	got, err := os.ReadFile(filepath.Join(source, "file"))
	if err != nil || string(got) != "original" {
		t.Fatal("copy mutated source", err)
	}
}

func TestSandboxResignsAfterGateMutation(t *testing.T) {
	if os.Getenv("PEKIT_TEST_SANDBOX") != "1" {
		t.Skip("set PEKIT_TEST_SANDBOX=1 on a host with unprivileged namespaces")
	}
	image := sandboxFixtureRoot(t)
	ws := t.TempDir()
	recipe := filepath.Join(ws, "recipe")
	writeFile(t, filepath.Join(ws, "workspace.pekit.toml"), "include=['recipe']\n[isolation]\nenabled=true\n")
	writeFile(t, filepath.Join(ws, "test.env.pekit.toml"), "[sandbox]\ncommand="+fmt.Sprintf("%q", "cp -a "+shellQuote(image+"/.")+" \"$PEKIT_SANDBOX_ROOT\"")+"\n")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
[build.main]
command='test -z "${PEKIT_KEYRING_TCB_PRIV:-}"; printf original > "$PEKIT_OUT/blob"'
[build.main.sign.pip]
blob="tcb.priv"
[test.main]
gate=true
needs=["main"]
command='printf changed > "$PEKIT_MAIN_OUT/blob"'
`)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), `
format="peipkg"
[package]
name="test.example"
version="1.0.0-1"
architecture="noarch"
license="MIT"
description="Isolated signing regression fixture"
[files]
":blob"="usr/share/test/blob"
":blob.peios.sig"="usr/share/test/blob.peios.sig"
`)
	keyPath := filepath.Join(t.TempDir(), "private.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{7}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := loadPIPSigningKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"package", "--env=test", "--keyring.tcb.priv=" + keyPath}); err != nil {
		t.Fatalf("%v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(recipe, "out/build/main/blob"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(filepath.Join(recipe, "out/build/main/blob.peios.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "changed" {
		t.Fatal("gate did not mutate prerequisite")
	}
	if err := verifyPIPDetached(data, sig, key.pub); err != nil {
		t.Fatalf("packaged signature was stale after gate: %v", err)
	}
}
