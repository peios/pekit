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
	if f.keyring["PEKIT_KEYRING_TCB_PRIV"] != "/secure/key.pem" {
		t.Errorf("priv = %q, want /secure/key.pem", f.keyring["PEKIT_KEYRING_TCB_PRIV"])
	}
	if f.keyring["PEKIT_KEYRING_TCB_PUB"] != "ABCD" {
		t.Errorf("pub = %q, want ABCD", f.keyring["PEKIT_KEYRING_TCB_PUB"])
	}

	// A value may contain '='.
	_, f, err = extractFlags([]string{"build", "--keyring.token=a=b=c"})
	if err != nil || f.keyring["PEKIT_KEYRING_TOKEN"] != "a=b=c" {
		t.Errorf("value-with-= parsed as %q (err %v)", f.keyring["PEKIT_KEYRING_TOKEN"], err)
	}

	// Missing value / missing name are errors.
	if _, _, err := extractFlags([]string{"build", "--keyring.tcb.priv"}); err == nil {
		t.Error("--keyring.<name> with no =value should error")
	}
	if _, _, err := extractFlags([]string{"build", "--keyring.=v"}); err == nil {
		t.Error("--keyring.=v with an empty name should error")
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
