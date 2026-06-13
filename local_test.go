package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalVersionSentinel(t *testing.T) {
	v := localVersion()
	if v.Full != "0.0.0-localdev" || v.Major != "0" || v.Minor != "0" || v.Patch != "0" || v.Prerelease != "localdev" {
		t.Errorf("localVersion = %+v", v)
	}
	// Every template variable must resolve (no {{...}} left undefined).
	for _, key := range []string{"version", "major", "minor", "patch", "prerelease", "buildmeta"} {
		if _, ok := v.lookup(key); !ok {
			t.Errorf("lookup(%q) not resolved by sentinel", key)
		}
	}
}

func TestRevScopeLocal(t *testing.T) {
	if got := revScope(&Source{Local: true, LocalPath: "../x", Git: "u", Rev: "v1"}); got != "localdev" {
		t.Errorf("revScope(local) = %q, want localdev", got)
	}
	if got := revScope(&Source{Rev: "refs/tags/v1.2"}); got != "refs_tags_v1.2" {
		t.Errorf("revScope(git) = %q", got)
	}
}

func TestApplyLocal(t *testing.T) {
	// No --local: no-op even without a source.
	if err := applyLocal(&Config{}, false); err != nil {
		t.Errorf("applyLocal(false) = %v, want nil", err)
	}
	// --local with no source.
	if err := applyLocal(&Config{}, true); err == nil || !strings.Contains(err.Error(), "needs a [source]") {
		t.Errorf("want no-source error, got: %v", err)
	}
	// --local with a source lacking localpath.
	cfg := &Config{Source: &Source{Git: "u", Rev: "v1"}}
	if err := applyLocal(cfg, true); err == nil || !strings.Contains(err.Error(), "no localpath") {
		t.Errorf("want no-localpath error, got: %v", err)
	}
	// --local with localpath: flips Local on.
	cfg = &Config{Source: &Source{LocalPath: "../x"}}
	if err := applyLocal(cfg, true); err != nil {
		t.Fatalf("applyLocal = %v", err)
	}
	if !cfg.Source.Local {
		t.Error("applyLocal did not set Source.Local")
	}
}

func TestLocalUsable(t *testing.T) {
	dir := t.TempDir()
	// existing directory → usable
	if !localUsable(&Source{LocalPath: dir}) {
		t.Error("existing dir localpath should be usable")
	}
	// no localpath → not usable (triggers remote fallback)
	if localUsable(&Source{Git: "u", Rev: "v1"}) {
		t.Error("no localpath should not be usable")
	}
	// nonexistent path → not usable
	if localUsable(&Source{LocalPath: filepath.Join(dir, "missing")}) {
		t.Error("missing localpath should not be usable")
	}
	// a file (not a directory) → not usable
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if localUsable(&Source{LocalPath: f}) {
		t.Error("a file localpath should not be usable (must be a dir)")
	}
}

func TestFetchSourceLocal(t *testing.T) {
	dir := t.TempDir()
	got, err := fetchSource(&Source{Local: true, LocalPath: dir}, "ignored/checkout/path")
	if err != nil {
		t.Fatalf("fetchSource(local) = %v", err)
	}
	want, _ := filepath.Abs(dir)
	if got != want {
		t.Errorf("fetchSource(local) = %q, want %q", got, want)
	}
	// A missing localpath is an error.
	if _, err := fetchSource(&Source{Local: true, LocalPath: filepath.Join(dir, "nope")}, ""); err == nil {
		t.Error("want error for missing localpath")
	}
}
