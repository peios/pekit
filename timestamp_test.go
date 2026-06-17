package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// echoVarRecipe builds a cfg whose single build target writes one env var to a
// file under PEKIT_OUT, so a test can read back what pekit injected.
func echoVarRecipe(v string) *Config {
	return &Config{
		OutDir:   "out",
		ClearOut: true,
		Commands: map[string]map[string]Target{
			"build": {"main": {Command: `echo "$` + v + `" > "$PEKIT_OUT/v"`}},
		},
	}
}

func readBuiltVar(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("out", "build", "main", "v"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestPekitBuildTimestamp(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runTarget(echoVarRecipe("PEKIT_BUILD_TIMESTAMP"), "build", "main", nil, noBuildSet{}); err != nil {
		t.Fatal(err)
	}
	if got, want := readBuiltVar(t), fmt.Sprintf("%d", pekitStart.Unix()); got != want {
		t.Errorf("PEKIT_BUILD_TIMESTAMP = %q, want %q (the run's start time)", got, want)
	}
}

func TestPekitSourceTimestampGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE=@1700000000 +0000", "GIT_COMMITTER_DATE=@1700000000 +0000",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "x")

	t.Chdir(dir)
	if err := runTarget(echoVarRecipe("PEKIT_SOURCE_TIMESTAMP"), "build", "main", nil, noBuildSet{}); err != nil {
		t.Fatal(err)
	}
	if got := readBuiltVar(t); got != "1700000000" {
		t.Errorf("PEKIT_SOURCE_TIMESTAMP = %q, want the commit's 1700000000", got)
	}
}

func TestPekitSourceTimestampNonGit(t *testing.T) {
	t.Chdir(t.TempDir()) // not a git repo
	if err := runTarget(echoVarRecipe("PEKIT_SOURCE_TIMESTAMP"), "build", "main", nil, noBuildSet{}); err != nil {
		t.Fatal(err)
	}
	if got := readBuiltVar(t); got != "0" {
		t.Errorf("PEKIT_SOURCE_TIMESTAMP = %q, want 0 (epoch) outside a git repo", got)
	}
}

func TestPekitTimestampsReservedInEnv(t *testing.T) {
	for _, n := range []string{"PEKIT_BUILD_TIMESTAMP", "PEKIT_SOURCE_TIMESTAMP"} {
		if _, err := ParseConfig("[env]\n" + n + " = \"x\"\n"); err == nil || !strings.Contains(err.Error(), n) {
			t.Errorf("%s should be reserved in [env]: err = %v", n, err)
		}
	}
}
