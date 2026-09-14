package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestIsolatedOutputCleanupWithoutEnvironment(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "workspace.pekit.toml"), "include=['a','b']\n[isolation]\nenabled=true\n")
	for _, name := range []string{"a", "b"} {
		writeFile(t, filepath.Join(root, name, "pekit.toml"), "out_dir='out'\n[clean]\ncommand='exit 93'\n")
		writeFile(t, filepath.Join(root, name, "out", "old"), "stale output")
	}
	writeFile(t, filepath.Join(root, "b", "pekit.toml"), "out_dir='out'\n[delegate]\nenv=true\n[source.local]\npath='../missing-source'\n")
	chdir(t, root)
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	recipe, err := LoadRecipe(filepath.Join(root, "a", "pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockBuildJob(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run([]string{"--recipe", "a", "clean", "--output-only"}); err == nil {
		t.Fatal("cleanup ignored active build lock")
	}
	if _, err := os.Stat(filepath.Join(root, "a", "out", "old")); err != nil {
		t.Fatal("locked output changed", err)
	}
	unlock()
	if err := app.Run([]string{"workspace", "clean", "--output-only"}); err != nil {
		t.Fatalf("%v: %s", err, output.String())
	}
	for _, name := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(root, name, "out")); !os.IsNotExist(err) {
			t.Fatalf("output retained: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, name, "pekit.toml")); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Run([]string{"--recipe", "a", "clean", "--target-only"}); err == nil {
		t.Fatal("clean target escaped environment requirement")
	}
}
