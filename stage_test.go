package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareOutDirCreatesTargetDir(t *testing.T) {
	root := t.TempDir()
	dir, err := prepareOutDir(filepath.Join(root, "out"), "app1", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("returned dir %q is not absolute", dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("staging dir not created: %v", err)
	}
}

func TestPrepareOutDirClears(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	stale := filepath.Join(out, "main", "stale.bin")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := prepareOutDir(out, "main", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.bin")); !os.IsNotExist(err) {
		t.Error("stale artifact survived clearOut=true")
	}
}

func TestPrepareOutDirPreservesWhenClearOff(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	kept := filepath.Join(out, "main", "kept.bin")
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := prepareOutDir(out, "main", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "kept.bin")); err != nil {
		t.Error("artifact lost despite clearOut=false")
	}
}

func TestPrepareOutDirIsolatesTargets(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	other := filepath.Join(out, "app2", "other.bin")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareOutDir(out, "app1", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("clearing app1 touched app2's staging dir")
	}
}
