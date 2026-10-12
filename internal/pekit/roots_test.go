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

func TestLocateRecipePrefersExistingLocalGitName(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cwd := t.TempDir()
			name := "com.git-scm.git"
			root := filepath.Join(cwd, name)
			path := filepath.Join(root, "pekit.toml")
			switch kind {
			case "directory":
				writeFile(t, path, `out_dir = "out"`)
			case "file":
				path = root
				root = cwd
				writeFile(t, path, `out_dir = "out"`)
			case "symlink":
				target := filepath.Join(cwd, "actual")
				writeFile(t, filepath.Join(target, "pekit.toml"), `out_dir = "out"`)
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			}
			got, _, err := locateRecipe(Invocation{Cwd: cwd, RecipeFlag: name})
			if err != nil {
				t.Fatal(err)
			}
			if got.Remote || got.Root != root || got.Path != path {
				t.Fatalf("local recipe = %+v, want root=%s path=%s", got, root, path)
			}
		})
	}
}

func TestLocateWorkspacePrefersExistingLocalGitName(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, "recipes.git")
	writeFile(t, filepath.Join(root, "workspace.pekit.toml"), `include = ["./*"]`)
	got, err := locateWorkspace(Invocation{Cwd: cwd, WorkspaceFlag: "recipes.git"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != root {
		t.Fatalf("root=%s, want %s", got.Root, root)
	}
}

func TestRemoteLocatorWithoutLocalMatch(t *testing.T) {
	cwd := t.TempDir()
	for _, value := range []string{"com.git-scm.git", "github.com/acme/tools", "https://git.example.test/tools.git", "tools//pkg"} {
		if !looksRemoteLocator(cwd, value) {
			t.Errorf("remote spelling %q no longer recognized", value)
		}
	}
	for _, value := range []string{"", "./com.git-scm.git", "/missing.git", "plain-recipe"} {
		if looksRemoteLocator(cwd, value) {
			t.Errorf("local spelling %q classified remote", value)
		}
	}
}
