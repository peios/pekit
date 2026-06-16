package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractFlagsKeyring(t *testing.T) {
	rest, f, err := extractFlags([]string{"build", "--keyring.tcb.priv=/secure/key.pem", "--keyring.tcb.pub=ABCD"})
	if err != nil {
		t.Fatalf("extractFlags: %v", err)
	}
	if len(rest) != 1 || rest[0] != "build" {
		t.Errorf("rest = %v, want [build]", rest)
	}
	if f.keyring.vars["PEKIT_KEYRING_TCB_PRIV"] != "/secure/key.pem" {
		t.Errorf("priv = %q, want /secure/key.pem", f.keyring.vars["PEKIT_KEYRING_TCB_PRIV"])
	}
	if f.keyring.vars["PEKIT_KEYRING_TCB_PUB"] != "ABCD" {
		t.Errorf("pub = %q, want ABCD", f.keyring.vars["PEKIT_KEYRING_TCB_PUB"])
	}

	// A value may contain '='.
	_, f, err = extractFlags([]string{"build", "--keyring.token=a=b=c"})
	if err != nil || f.keyring.vars["PEKIT_KEYRING_TOKEN"] != "a=b=c" {
		t.Errorf("value-with-= parsed as %q (err %v)", f.keyring.vars["PEKIT_KEYRING_TOKEN"], err)
	}

	// --keyring=<file> records a file to load.
	_, f, err = extractFlags([]string{"build", "--keyring=prod"})
	if err != nil || len(f.keyring.files) != 1 || f.keyring.files[0] != "prod" {
		t.Errorf("--keyring=prod files=%v err=%v", f.keyring.files, err)
	}
	if _, _, err := extractFlags([]string{"build", "--keyring="}); err == nil {
		t.Error("--keyring= with an empty name should error")
	}

	// Missing value / missing name are errors.
	if _, _, err := extractFlags([]string{"build", "--keyring.tcb.priv"}); err == nil {
		t.Error("--keyring.<name> with no =value should error")
	}
	if _, _, err := extractFlags([]string{"build", "--keyring.=v"}); err == nil {
		t.Error("--keyring.=v with an empty name should error")
	}
}

func TestKeyringFileFlattenAndPrecedence(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("x.keyring.pekit.toml", []byte(`
[tcb]
priv.path = "/secrets/tcb.pem"
pub = "DEAD"

[otherkey]
priv = "P"
pub = "Q"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// --keyring=x flattens nested keys to PEKIT_KEYRING_<DOTTED_PATH>.
	m, err := resolveKeyring(keyringSpec{files: []string{"x"}})
	if err != nil {
		t.Fatalf("resolveKeyring: %v", err)
	}
	want := map[string]string{
		"PEKIT_KEYRING_TCB_PRIV_PATH": "/secrets/tcb.pem",
		"PEKIT_KEYRING_TCB_PUB":       "DEAD",
		"PEKIT_KEYRING_OTHERKEY_PRIV": "P",
		"PEKIT_KEYRING_OTHERKEY_PUB":  "Q",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}

	// An individual --keyring flag overrides a file value.
	m, err = resolveKeyring(keyringSpec{
		files: []string{"x"},
		vars:  map[string]string{"PEKIT_KEYRING_TCB_PUB": "BEEF"},
	})
	if err != nil {
		t.Fatalf("resolveKeyring: %v", err)
	}
	if m["PEKIT_KEYRING_TCB_PUB"] != "BEEF" {
		t.Errorf("override = %q, want BEEF (CLI must beat file)", m["PEKIT_KEYRING_TCB_PUB"])
	}

	// A missing file errors.
	if _, err := resolveKeyring(keyringSpec{files: []string{"nope"}}); err == nil {
		t.Error("a missing keyring file should error")
	}

	// A non-string leaf errors.
	if err := os.WriteFile("bad.keyring.pekit.toml", []byte("[k]\nn = 5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveKeyring(keyringSpec{files: []string{"bad"}}); err == nil {
		t.Error("a non-string keyring leaf should error")
	}
}

func TestKeyringSearchDirs(t *testing.T) {
	root := t.TempDir()
	member := filepath.Join(root, "m1")
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}
	// File only at the workspace root → found via the second search dir.
	if err := os.WriteFile(filepath.Join(root, "prod.keyring.pekit.toml"), []byte("[tcb]\npub = \"ROOT\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := resolveKeyring(keyringSpec{files: []string{"prod"}}, member, root)
	if err != nil || m["PEKIT_KEYRING_TCB_PUB"] != "ROOT" {
		t.Fatalf("root search = %v (err %v), want ROOT", m, err)
	}
	// A member-local file wins over the root.
	if err := os.WriteFile(filepath.Join(member, "prod.keyring.pekit.toml"), []byte("[tcb]\npub = \"LOCAL\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err = resolveKeyring(keyringSpec{files: []string{"prod"}}, member, root)
	if err != nil || m["PEKIT_KEYRING_TCB_PUB"] != "LOCAL" {
		t.Fatalf("member precedence = %v (err %v), want LOCAL", m, err)
	}
}

func TestKeyringDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "workspace.pekit.toml"), []byte("include = \"./*\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	member := filepath.Join(root, "m1")
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatal(err)
	}

	// From a member: search the current dir, then the workspace root.
	t.Chdir(member)
	dirs, err := keyringDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || dirs[0] != "." {
		t.Fatalf("member dirs = %v, want [. <root>]", dirs)
	}
	if _, err := os.Stat(filepath.Join(dirs[1], "workspace.pekit.toml")); err != nil {
		t.Errorf("dirs[1] = %s should be the workspace root", dirs[1])
	}

	// At the workspace root itself: just the current dir (no duplicate).
	t.Chdir(root)
	dirs, err = keyringDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "." {
		t.Errorf("root dirs = %v, want [.]", dirs)
	}

	// Outside any workspace: just the current dir.
	t.Chdir(t.TempDir())
	dirs, err = keyringDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "." {
		t.Errorf("no-workspace dirs = %v, want [.]", dirs)
	}
}

// TestRemoteSpecIgnoresFlags: a pathy --keyring value must not be mistaken for
// a remote recipe spec (extractRemoteSpec runs on raw args, before flags are
// parsed).
func TestRemoteSpecIgnoresFlags(t *testing.T) {
	_, _, ok := extractRemoteSpec([]string{"package", "--keyring.tcb.priv=/run/secrets/tcb.pem"})
	if ok {
		t.Error("a --keyring flag value was misread as a remote recipe spec")
	}
	// A real remote spec after flags is still detected.
	_, spec, ok := extractRemoteSpec([]string{"package", "--env", "none", "github.com/peios/loregd"})
	if !ok || spec != "github.com/peios/loregd" {
		t.Errorf("remote spec after flags: ok=%v spec=%q", ok, spec)
	}
}

// TestKeyringInjectedAndSurvivesWrap: a keyring value is exported into the
// command and, because it is baked into the script text, survives a wrapper
// that scrubs the environment (docker / nix-shell --pure, simulated by env -i).
func TestKeyringInjectedAndSurvivesWrap(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &Config{
		OutDir:   "out",
		ClearOut: true,
		Wrap:     "env -i sh -euc {{command}}",
		Keyring:  map[string]string{"PEKIT_KEYRING_TCB_PRIV": "/secure/key.pem"},
		Commands: map[string]map[string]Target{
			"build": {"main": {Command: `echo "$PEKIT_KEYRING_TCB_PRIV" > "$PEKIT_OUT/k"`}},
		},
	}
	if err := runTarget(cfg, "build", "main", nil, noBuildSet{}); err != nil {
		t.Fatalf("keyring build failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("out", "build", "main", "k"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "/secure/key.pem" {
		t.Errorf("got %q, want /secure/key.pem", strings.TrimSpace(string(data)))
	}
}

// TestKeyringRequiredViaNounset: a command that references an unprovided
// keyring var fails, because commands run under `sh -u` — this is how "error
// until provided" works without any explicit detection.
func TestKeyringRequiredViaNounset(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &Config{
		OutDir:   "out",
		ClearOut: true,
		Commands: map[string]map[string]Target{
			"build": {"main": {Command: `echo "$PEKIT_KEYRING_MISSING" > "$PEKIT_OUT/k"`}},
		},
	}
	if err := runTarget(cfg, "build", "main", nil, noBuildSet{}); err == nil {
		t.Error("a command using an unprovided keyring var must fail (sh -u unbound variable)")
	}
}
