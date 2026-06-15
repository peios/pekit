package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsRemoteSpec(t *testing.T) {
	cases := map[string]bool{
		"github.com/peios/myapp":            true,
		"github.com/peios/mono/pkgs/glibc":  true,
		"https://github.com/peios/app.git":  true,
		"git@github.com:peios/app.git":      true,
		"git.example.org/team/app":          true,
		"main":                              false, // bare target name
		"client":                            false,
		"./local/dir":                       false,
		"../sibling":                        false,
		"/abs/path":                         false,
		"~/home/dir":                        false,
		"localhost/app":                     false, // no dot in host
	}
	for in, want := range cases {
		if got := isRemoteSpec(in); got != want {
			t.Errorf("isRemoteSpec(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseRemoteSpec(t *testing.T) {
	cases := []struct {
		spec, url, subdir, ref string
	}{
		{"github.com/peios/myapp", "https://github.com/peios/myapp.git", "", ""},
		{"github.com/peios/mono/pkgs/glibc", "https://github.com/peios/mono.git", "pkgs/glibc", ""},
		{"github.com/peios/myapp@v1.2.3", "https://github.com/peios/myapp.git", "", "v1.2.3"},
		{"github.com/peios/mono/pkgs/glibc@main", "https://github.com/peios/mono.git", "pkgs/glibc", "main"},
		{"https://example.org/x/y.git", "https://example.org/x/y.git", "", ""},
		{"git@github.com:peios/app.git", "git@github.com:peios/app.git", "", ""},
	}
	for _, c := range cases {
		url, subdir, ref, err := parseRemoteSpec(c.spec)
		if err != nil {
			t.Errorf("parseRemoteSpec(%q): %v", c.spec, err)
			continue
		}
		if url != c.url || subdir != c.subdir || ref != c.ref {
			t.Errorf("parseRemoteSpec(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.spec, url, subdir, ref, c.url, c.subdir, c.ref)
		}
	}
}

func TestParseRemoteSpecErrors(t *testing.T) {
	for _, spec := range []string{"github.com", "github.com/peios/app@"} {
		if _, _, _, err := parseRemoteSpec(spec); err == nil {
			t.Errorf("parseRemoteSpec(%q) should error", spec)
		}
	}
}

func TestExtractRemoteSpec(t *testing.T) {
	// Spec is pulled out, verb and flags survive.
	rest, spec, ok := extractRemoteSpec([]string{"install", "github.com/peios/app", "--version", "1.2"})
	if !ok || spec != "github.com/peios/app" {
		t.Fatalf("extract = (%v,%q,%v)", rest, spec, ok)
	}
	if strings.Join(rest, " ") != "install --version 1.2" {
		t.Errorf("rest = %v, want [install --version 1.2]", rest)
	}
	// A versionish value after --version is not mistaken for a spec.
	if _, _, ok := extractRemoteSpec([]string{"build", "--version", "1.2.3"}); ok {
		t.Error("a --version value must not be read as a remote spec")
	}
	// A local target name is not a spec.
	if _, _, ok := extractRemoteSpec([]string{"install", "main"}); ok {
		t.Error("a bare target is not a remote spec")
	}
	// Only per-recipe verbs are eligible.
	if _, _, ok := extractRemoteSpec([]string{"frobnicate", "github.com/peios/app"}); ok {
		t.Error("a non-verb should not trigger remote handling")
	}
}

func TestRemoteCacheDir(t *testing.T) {
	got := remoteCacheDir("https://github.com/peios/myapp.git")
	want := filepath.Join(os.TempDir(), "pekit", "remote", "github.com", "peios", "myapp")
	if got != want {
		t.Errorf("remoteCacheDir = %q, want %q", got, want)
	}
}

// initRepo makes a throwaway git repo at dir with the given files committed.
func initRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestRunRemoteInstall drives the whole path with a local git repo as the
// "remote" (file:// URL): clone → run install in the checkout → delete it.
func TestRunRemoteInstall(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	sentinel := filepath.Join(t.TempDir(), "installed")
	initRepo(t, origin, map[string]string{
		"pekit.toml": "[install]\ncommand = \"echo done > '" + sentinel + "'\"\n",
	})

	spec := "file://" + origin
	dest := remoteCacheDir(spec)
	t.Cleanup(func() { os.RemoveAll(dest) })

	if err := run([]string{"install", spec}); err != nil {
		t.Fatalf("run install %s: %v", spec, err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("install command did not run in the checkout: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("checkout %s should be removed after the verb finishes (err=%v)", dest, err)
	}
}

func TestRunRemoteNoPekitToml(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin")
	initRepo(t, origin, map[string]string{"README": "no recipe here\n"})
	spec := "file://" + origin
	t.Cleanup(func() { os.RemoveAll(remoteCacheDir(spec)) })

	err := run([]string{"install", spec})
	if err == nil || !strings.Contains(err.Error(), "no pekit.toml") {
		t.Fatalf("err = %v, want a 'no pekit.toml' error", err)
	}
}
