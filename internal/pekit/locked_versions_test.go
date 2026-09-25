package pekit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// lockedFixture makes an upstream git repository tagged v1.0 and v1.1, a
// recipe tracking it, and pins only 1.0 in the recipe's lock.
func lockedFixture(t *testing.T) (repo, recipe string) {
	t.Helper()
	dir := t.TempDir()
	repo = filepath.Join(dir, "src")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "payload.txt"), "one")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "one")
	runTestCmd(t, repo, "git", "tag", "v1.0")
	writeFile(t, filepath.Join(repo, "payload.txt"), "two")
	runTestCmd(t, repo, "git", "commit", "-am", "two")
	runTestCmd(t, repo, "git", "tag", "v1.1")

	recipe = filepath.Join(dir, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "v{{version}}"

[build]
command = 'printf "$PEKIT_VERSION" > "$PEKIT_OUT/version.txt"'
`)
	chdir(t, recipe)
	return repo, recipe
}

func builtVersions(t *testing.T, recipe string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(recipe, "out", "git-*", "build", "main", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(data))
	}
	return out
}

// --latest --locked selects the newest locked version, not the newest tag
// upstream, and leaves the lock as it was.
func TestLockedLatestSelectsFromTheLock(t *testing.T) {
	_, recipe := lockedFixture(t)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"lock", "--version", "1.0"}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := app.Run([]string{"build", "--latest", "--locked"}); err != nil {
		t.Fatalf("build --latest --locked: %v", err)
	}
	if got := builtVersions(t, recipe); len(got) != 1 || got[0] != "1.0" {
		t.Fatalf("built %v, want only the locked 1.0", got)
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Sources) != 1 || lock.Find("1.1") != nil {
		t.Fatalf("--locked changed the lock: %+v", lock.Sources)
	}
}

// A constraint under --locked matches locked versions only.
func TestLockedConstraintMatchesOnlyLockedVersions(t *testing.T) {
	_, recipe := lockedFixture(t)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"lock", "--version", "1.0"}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	err := app.Run([]string{"build", "--version", ">= 1.1", "--locked"})
	if diagCode(err) != "version_selection_empty" {
		t.Fatalf("build -V '>= 1.1' --locked = %v, want version_selection_empty", err)
	}
	if got := builtVersions(t, recipe); len(got) != 0 {
		t.Fatalf("built %v, want nothing", got)
	}
}

func TestLockedWithNoLockEntriesFails(t *testing.T) {
	lockedFixture(t)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"build", "--latest", "--locked"}); diagCode(err) != "version_selection_empty" {
		t.Fatalf("build --latest --locked with no lock = %v, want version_selection_empty", err)
	}
}

func TestLockedFlagCombinations(t *testing.T) {
	lockedFixture(t)
	for name, args := range map[string][]string{
		"alone":         {"build", "--locked"},
		"exact version": {"build", "--version", "1.0", "--locked"},
		"lock command":  {"lock", "--latest", "--locked"},
	} {
		t.Run(name, func(t *testing.T) {
			app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
			if err := app.Run(args); diagCode(err) != "invalid_flags" {
				t.Fatalf("%v = %v, want invalid_flags", args, err)
			}
		})
	}
}
