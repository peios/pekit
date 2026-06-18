package pekit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRemoteSSHRef(t *testing.T) {
	spec, err := parseRemote("git@github.com:org/recipes.git@main")
	if err != nil {
		t.Fatalf("parse remote: %v", err)
	}
	if spec.URL != "git@github.com:org/recipes.git" || spec.Ref != "main" || spec.Subdir != "" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
}

func TestMaterializeRemoteLocatorUsesResolvedCommitCache(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "recipes.git")
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "pkg", "pekit.toml"), `out_dir = "out"`)
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	runTestCmd(t, repo, "git", "tag", "v1")
	commitBytes, err := os.ReadFile(filepath.Join(repo, ".git", "refs", "heads", "master"))
	if err != nil {
		commitBytes, err = os.ReadFile(filepath.Join(repo, ".git", "refs", "heads", "main"))
		if err != nil {
			t.Fatal(err)
		}
	}
	commit := string(commitBytes)
	if len(commit) > 40 {
		commit = commit[:40]
	}
	byTag, err := materializeRemoteLocator(repo+"//pkg@v1", "recipe")
	if err != nil {
		t.Fatalf("materialize by tag: %v", err)
	}
	byCommit, err := materializeRemoteLocator(repo+"//pkg@"+commit, "recipe")
	if err != nil {
		t.Fatalf("materialize by commit: %v", err)
	}
	if byTag != byCommit {
		t.Fatalf("expected same commit-keyed checkout, tag=%s commit=%s", byTag, byCommit)
	}
}
