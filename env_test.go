package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWrap(t *testing.T) {
	// Valid.
	got, err := parseWrap("env.pekit.toml", `[wrap]
command = "docker run --rm pkm-build sh -euc {{command}}"
`)
	if err != nil {
		t.Fatalf("valid wrap rejected: %v", err)
	}
	if !strings.Contains(got, "{{command}}") {
		t.Errorf("parseWrap returned %q, want the template verbatim", got)
	}

	// A wrap that drops {{command}} would silently discard the build.
	if _, err := parseWrap("env.pekit.toml", `[wrap]
command = "docker run pkm-build true"
`); err == nil || !strings.Contains(err.Error(), "{{command}}") {
		t.Errorf("err = %v, want a missing-{{command}} error", err)
	}

	// Strict: only [wrap].command is allowed.
	if _, err := parseWrap("env.pekit.toml", `[other]
x = 1
`); err == nil || !strings.Contains(err.Error(), "unknown section") {
		t.Errorf("err = %v, want an unknown-section error", err)
	}
	if _, err := parseWrap("env.pekit.toml", `[wrap]
command = "x {{command}}"
extra = "y"
`); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("err = %v, want an unknown-key error", err)
	}
	if _, err := parseWrap("env.pekit.toml", `[wrap]
command = ""
`); err == nil {
		t.Error("an empty command should error")
	}
}

func TestLoadWrap(t *testing.T) {
	t.Chdir(t.TempDir())

	// none → never wraps, never reads a file.
	if w, err := loadWrap("none"); err != nil || w != "" {
		t.Errorf("none = (%q,%v), want (\"\",nil)", w, err)
	}

	// main with no env.pekit.toml → silent no-op (main is the default).
	if w, err := loadWrap("main"); err != nil || w != "" {
		t.Errorf("missing main = (%q,%v), want (\"\",nil)", w, err)
	}

	// An explicitly-named env that is missing → error.
	if _, err := loadWrap("ci"); err == nil {
		t.Error("a missing named env file should error")
	}

	// main with a present env.pekit.toml → its wrap.
	if err := os.WriteFile("env.pekit.toml", []byte("[wrap]\ncommand = \"w {{command}}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, err := loadWrap("main"); err != nil || w != "w {{command}}" {
		t.Errorf("main = (%q,%v), want (\"w {{command}}\",nil)", w, err)
	}

	// <name> resolves to <name>.env.pekit.toml.
	if err := os.WriteFile("ci.env.pekit.toml", []byte("[wrap]\ncommand = \"ci {{command}}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, err := loadWrap("ci"); err != nil || w != "ci {{command}}" {
		t.Errorf("ci = (%q,%v), want (\"ci {{command}}\",nil)", w, err)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"abc":  `'abc'`,
		"a b":  `'a b'`,
		"it's": `'it'\''s'`,
		"":     `''`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWrapBakesPekitOutThroughScrubbedEnv is the core guarantee: PEKIT_OUT is
// baked into the script text, so it survives a wrapper that scrubs the inherited
// environment (the docker run / nix-shell --pure case, simulated with `env -i`).
func TestWrapBakesPekitOutThroughScrubbedEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &Config{
		OutDir:   "out",
		ClearOut: true,
		// env -i drops all inherited env vars before running the inner script,
		// exactly as a container boundary would; only baked-in exports survive.
		Wrap: "env -i sh -euc {{command}}",
		Commands: map[string]map[string]Target{
			"build": {"main": {Command: `echo ok > "$PEKIT_OUT/marker"`}},
		},
	}
	if err := runTarget(cfg, "build", "main", nil, noBuildSet{}); err != nil {
		t.Fatalf("wrapped build failed: %v", err)
	}
	marker := filepath.Join("out", "build", "main", "marker")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("PEKIT_OUT did not survive the env-scrubbing wrap: %v", err)
	}
}

// TestWrapHandlesSingleQuotedCommand: a command containing single quotes (e.g.
// glibc's `echo 'rootsbindir=…'`) must run correctly when shell-quoted into the
// wrap template.
func TestWrapHandlesSingleQuotedCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := &Config{
		OutDir:   "out",
		ClearOut: true,
		Wrap:     "sh -euc {{command}}",
		Commands: map[string]map[string]Target{
			"build": {"main": {Command: `echo 'a'\''b' > "$PEKIT_OUT/q"`}},
		},
	}
	if err := runTarget(cfg, "build", "main", nil, noBuildSet{}); err != nil {
		t.Fatalf("wrapped build with quotes failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join("out", "build", "main", "q"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "a'b" {
		t.Errorf("got %q, want \"a'b\"", strings.TrimSpace(string(data)))
	}
}
