package pekit

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeTrackedGitFixture(t *testing.T) (root, repo, recipe string, app *App) {
	t.Helper()
	root = t.TempDir()
	repo = filepath.Join(root, "upstream")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init", "-b", "release")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "security", "trust", "certdata.txt"), "trust-v1\n")
	writeFile(t, filepath.Join(repo, "unrelated.txt"), "unrelated-v1\n")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")

	recipe = filepath.Join(root, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "`+repo+`"
ref = "refs/heads/release"
tracked_path = "security/trust/certdata.txt"

[source_package]
name = "app-source"

[build]
command = '''
test -f security/trust/certdata.txt
test ! -e unrelated.txt
mkdir -p "$PEKIT_OUT"
cp security/trust/certdata.txt "$PEKIT_OUT/value"
'''
`)
	var stdout, stderr bytes.Buffer
	app = &App{Stdout: &stdout, Stderr: &stderr, Now: func() time.Time {
		return time.Date(2026, 9, 6, 12, 0, 0, 0, time.FixedZone("test", -7*60*60))
	}}
	return root, repo, recipe, app
}

func commitTrackedFixture(t *testing.T, repo, path, content, message string) string {
	t.Helper()
	writeFile(t, filepath.Join(repo, filepath.FromSlash(path)), content)
	runTestCmd(t, repo, "git", "add", path)
	runTestCmd(t, repo, "git", "commit", "-m", message)
	return strings.TrimSpace(runTestCmdOutput(t, repo, "git", "rev-parse", "HEAD"))
}

func runTestCmdOutput(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out, err := commandOutput(dir, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runTrackedApp(t *testing.T, recipe string, app *App, args ...string) error {
	t.Helper()
	oldwd, _ := os.Getwd()
	if err := os.Chdir(recipe); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldwd) }()
	return app.Run(args)
}

func TestTrackedGitSnapshotUnchangedChangeSameDayAndHistory(t *testing.T) {
	_, repo, recipe, app := makeTrackedGitFixture(t)
	if err := runTrackedApp(t, recipe, app, "lock", "--latest"); err != nil {
		t.Fatalf("initial lock: %v", err)
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	first := lock.Find("2026.09.06")
	if first == nil || first.Repository != repo || first.Ref != "refs/heads/release" || first.Path != "security/trust/certdata.txt" || first.Commit == "" || first.Blob == "" || len(first.BlobSHA256) != 64 {
		t.Fatalf("incomplete initial tracked lock: %#v", first)
	}
	firstCommit := first.Commit

	// A moving branch commit that does not change the tracked blob creates no
	// package version and never rewrites the old commit provenance.
	commitTrackedFixture(t, repo, "unrelated.txt", "unrelated-v2\n", "unrelated")
	if err := runTrackedApp(t, recipe, app, "lock", "--latest"); err != nil {
		t.Fatalf("unchanged re-lock: %v", err)
	}
	lock, err = LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Sources) != 1 || lock.Find("2026.09.06").Commit != firstCommit {
		t.Fatalf("unchanged blob altered history: %#v", lock.Sources)
	}

	secondCommit := commitTrackedFixture(t, repo, "security/trust/certdata.txt", "trust-v2\n", "trust change")
	if err := runTrackedApp(t, recipe, app, "lock", "--latest"); err != nil {
		t.Fatalf("changed same-day lock: %v", err)
	}
	lock, err = LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	second := lock.Find("2026.09.06.2")
	if len(lock.Sources) != 2 || second == nil || second.Commit != secondCommit || second.BlobSHA256 == first.BlobSHA256 {
		t.Fatalf("same-day history = %#v", lock.Sources)
	}

	// --all-versions replays every lock and retains the whole append-only
	// history when the current blob is unchanged.
	if err := runTrackedApp(t, recipe, app, "lock", "--all-versions"); err != nil {
		t.Fatalf("lock history: %v", err)
	}
	lock, err = LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Sources) != 2 || lock.Find("2026.09.06") == nil || lock.Find("2026.09.06.2") == nil {
		t.Fatalf("--all-versions lost history: %#v", lock.Sources)
	}
}

func TestTrackedGitExactLockedBuildIgnoresMovedTipAndWorksOffline(t *testing.T) {
	root, repo, recipe, app := makeTrackedGitFixture(t)
	if err := runTrackedApp(t, recipe, app, "build", "--latest"); err != nil {
		t.Fatalf("initial build: %v", err)
	}
	commitTrackedFixture(t, repo, "security/trust/certdata.txt", "trust-v2\n", "move tip")
	if err := runTrackedApp(t, recipe, app, "build", "--version", "2026.09.06"); err != nil {
		t.Fatalf("exact build after moving tip: %v", err)
	}
	values, err := filepath.Glob(filepath.Join(recipe, "out", "git-tracked-*", "build", "main", "value"))
	if err != nil || len(values) == 0 {
		t.Fatalf("build values: %v (%v)", values, err)
	}
	for _, value := range values {
		data, readErr := os.ReadFile(value)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(data) != "trust-v1\n" {
			t.Fatalf("locked build followed tip: %q", data)
		}
	}

	// Removing the upstream proves an exact locked build does not fetch or
	// resolve the moving ref once its immutable objects are cached.
	offline := filepath.Join(root, "upstream-offline")
	if err := os.Rename(repo, offline); err != nil {
		t.Fatal(err)
	}
	if err := runTrackedApp(t, recipe, app, "build", "--version", "2026.09.06"); err != nil {
		t.Fatalf("offline exact build: %v", err)
	}
}

func TestTrackedGitExactCurrentSnapshotCanCreateInitialLock(t *testing.T) {
	_, _, recipe, app := makeTrackedGitFixture(t)
	if err := runTrackedApp(t, recipe, app, "lock", "--version", "2026.09.06"); err != nil {
		t.Fatalf("exact initial lock: %v", err)
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if entry := lock.Find("2026.09.06"); entry == nil || !entry.isTrackedGit() {
		t.Fatalf("exact selector did not pin current snapshot: %#v", entry)
	}
	if err := runTrackedApp(t, recipe, app, "lock", "--version", "2026.09.07"); err == nil || diagCode(err) != "tracked_git_version" {
		t.Fatalf("non-current exact version did not fail: %v", err)
	}
}

func TestTrackedGitRepinIsRejected(t *testing.T) {
	_, _, recipe, app := makeTrackedGitFixture(t)
	err := runTrackedApp(t, recipe, app, "lock", "--repin", "--version", "2026.09.06")
	if err == nil || diagCode(err) != "invalid_flags" {
		t.Fatalf("tracked snapshot repin did not fail: %v", err)
	}
}

func TestTrackedGitLockTamperAndMovedConfigurationFailClosed(t *testing.T) {
	_, _, recipe, app := makeTrackedGitFixture(t)
	if err := runTrackedApp(t, recipe, app, "lock", "--latest"); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLockFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock.Find("2026.09.06")
	entry.BlobSHA256 = strings.Repeat("0", 64)
	if err := SaveLockFile(recipe, lock); err != nil {
		t.Fatal(err)
	}
	err = runTrackedApp(t, recipe, app, "build", "--version", "2026.09.06")
	if err == nil || diagCode(err) != "lock_mismatch" {
		t.Fatalf("tampered digest did not fail closed: %v", err)
	}

	// Restore the digest, then prove a recipe path move cannot silently reuse
	// a lock made for another path.
	entry.BlobSHA256 = sha256Hex([]byte("trust-v1\n"))
	if err := SaveLockFile(recipe, lock); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(recipe, "pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(recipe, "pekit.toml"), strings.Replace(string(data), "security/trust/certdata.txt", "unrelated.txt", 1))
	err = runTrackedApp(t, recipe, app, "build", "--version", "2026.09.06")
	if err == nil || diagCode(err) != "lock_mismatch" {
		t.Fatalf("moved tracked_path did not fail closed: %v", err)
	}
}

func TestTrackedGitSourcePackageArchivesOnlyTrackedPath(t *testing.T) {
	_, _, recipe, app := makeTrackedGitFixture(t)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), strings.Replace(sourcePkgMemberDef, `"@source:payload.txt"`, `"@source:security/trust/certdata.txt"`, 1))
	if err := runTrackedApp(t, recipe, app, "package", "--latest"); err != nil {
		t.Fatalf("package: %v", err)
	}
	artifact := globOne(t, filepath.Join(recipe, "out", "git-tracked-*", "package", "*", "app-source_2026.09.06-1_noarch.peipkg"))
	entries := readPeipkgEntries(t, artifact)
	archive := entries["usr/src/dist/app-2026.09.06-1/upstream/app-2026.09.06-1.tar"]
	if len(archive) == 0 {
		t.Fatalf("missing tracked source archive: %v", sortedKeys(entriesPresent(entries)))
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = true
		if hdr.Name == "app-2026.09.06-1/security/trust/certdata.txt" {
			if hdr.Typeflag != tar.TypeReg || hdr.Mode != 0o644 {
				t.Fatalf("tracked archive mode/type = %#o/%d", hdr.Mode, hdr.Typeflag)
			}
			if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
				t.Fatalf("tracked archive owner = %d:%d %q:%q", hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
			}
			if hdr.ModTime.Unix() != 0 || !hdr.AccessTime.IsZero() || !hdr.ChangeTime.IsZero() {
				t.Fatalf("tracked archive timestamps are not normalized: %#v", hdr)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "trust-v1\n" {
				t.Fatalf("tracked archive data = %q", data)
			}
		}
	}
	if !seen["app-2026.09.06-1/security/trust/certdata.txt"] {
		t.Fatalf("tracked file absent from archive: %v", sortedKeys(seen))
	}
	for name := range seen {
		if strings.Contains(name, "unrelated.txt") || strings.Contains(name, ".git") {
			t.Fatalf("untracked repository content leaked into source archive: %s", name)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("tracked archive contains more than its one file: %v", sortedKeys(seen))
	}
}

func TestTrackedGitSourcePackageIsOfflineWithoutAttributeBlobsAndReproducible(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "upstream")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestCmd(t, repo, "git", "init", "-b", "release")
	runTestCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runTestCmd(t, repo, "git", "config", "user.name", "Test")
	runTestCmd(t, repo, "git", "config", "uploadpack.allowFilter", "true")
	writeFile(t, filepath.Join(repo, ".gitattributes"), "*.txt export-subst\n")
	writeFile(t, filepath.Join(repo, "security", "trust", "certdata.txt"), "trust-v1\n")
	writeFile(t, filepath.Join(repo, "unrelated.txt"), "unrelated-v1\n")
	runTestCmd(t, repo, "git", "add", ".")
	runTestCmd(t, repo, "git", "commit", "-m", "initial")
	attributeBlob := strings.TrimSpace(runTestCmdOutput(t, repo, "git", "rev-parse", "HEAD:.gitattributes"))

	recipe := filepath.Join(root, "recipe")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), `
out_dir = "out"

[source.git]
url = "file://`+filepath.ToSlash(repo)+`"
ref = "refs/heads/release"
tracked_path = "security/trust/certdata.txt"

[source_package]
name = "app-source"

[build]
command = "true"
`)
	writeFile(t, filepath.Join(recipe, "package.pekit.toml"), strings.Replace(sourcePkgMemberDef, `"@source:payload.txt"`, `"@source:security/trust/certdata.txt"`, 1))
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr, Now: func() time.Time {
		return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	}}
	if err := runTrackedApp(t, recipe, app, "lock", "--latest"); err != nil {
		t.Fatalf("lock: %v\nstderr=%s", err, stderr.String())
	}
	cfg, err := LoadRecipe(filepath.Join(recipe, "pekit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cache := trackedGitRepo(cfg, cfg.Source.Git)
	cmd := execCommand("git", "cat-file", "-e", attributeBlob)
	cmd.Dir = cache
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("fixture unexpectedly cached .gitattributes; it cannot prove offline git-archive avoidance")
	}

	if err := os.Rename(repo, filepath.Join(root, "upstream-offline")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_NO_LAZY_FETCH", "1")
	if err := runTrackedApp(t, recipe, app, "package", "--version", "2026.09.06"); err != nil {
		t.Fatalf("offline package: %v\nstderr=%s", err, stderr.String())
	}
	artifact := globOne(t, filepath.Join(recipe, "out", "git-tracked-*", "package", "*", "app-source_2026.09.06-1_noarch.peipkg"))
	first, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	entries := readPeipkgEntries(t, artifact)
	archive := entries["usr/src/dist/app-2026.09.06-1/upstream/app-2026.09.06-1.tar"]
	tr := tar.NewReader(bytes.NewReader(archive))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "app-2026.09.06-1/security/trust/certdata.txt" {
		t.Fatalf("offline archive entry = %q", hdr.Name)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("offline archive has an unexpected second entry: %v", err)
	}

	if err := runTrackedApp(t, recipe, app, "package", "--version", "2026.09.06"); err != nil {
		t.Fatalf("second offline package: %v\nstderr=%s", err, stderr.String())
	}
	second, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("offline tracked-path source package changed across identical runs: %s != %s", sha256Hex(first), sha256Hex(second))
	}
}

func TestTrackedGitSourceArchiveRejectsTraversalSymlinkAndTamper(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "certdata.txt")
	writeFile(t, file, "trust\n")
	base := SourceState{
		TrackedRoot:   root,
		TrackedPath:   "certdata.txt",
		TrackedSHA256: sha256Hex([]byte("trust\n")),
		TrackedMode:   0o644,
	}
	archive := filepath.Join(t.TempDir(), "source.tar")

	escape := base
	escape.TrackedPath = "../certdata.txt"
	if err := writeTrackedGitSourceArchive(archive, "app-1", escape); err == nil {
		t.Fatal("tracked archive accepted path traversal")
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere"), file); err != nil {
		t.Fatal(err)
	}
	if err := writeTrackedGitSourceArchive(archive, "app-1", base); err == nil {
		t.Fatal("tracked archive accepted a symlink")
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, "tampered\n")
	if err := writeTrackedGitSourceArchive(archive, "app-1", base); err == nil || diagCode(err) != "lock_mismatch" {
		t.Fatalf("tracked archive did not reject changed bytes: %v", err)
	}
}

func TestNextTrackedSnapshotVersionMonotonicAndDeterministic(t *testing.T) {
	ctx := &Context{Start: time.Date(2026, 9, 5, 23, 0, 0, 0, time.UTC)}
	history := []LockSource{{Version: "2026.09.06"}, {Version: "2026.09.06.2"}, {Version: "2026.09.06.4"}}
	got, err := nextTrackedSnapshotVersion(ctx, history)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026.09.06.5" {
		t.Fatalf("next version = %q, want 2026.09.06.5", got)
	}
}

func TestNextTrackedSnapshotVersionCrossesDecimalSuffixBoundaries(t *testing.T) {
	ctx := &Context{Start: time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		previous string
		want     string
	}{
		{previous: "2026.09.06.9", want: "2026.09.06.10"},
		{previous: "2026.09.06.19", want: "2026.09.06.20"},
		{previous: "2026.09.06.99", want: "2026.09.06.100"},
	} {
		got, err := nextTrackedSnapshotVersion(ctx, []LockSource{{Version: tc.previous}})
		if err != nil {
			t.Fatalf("after %s: %v", tc.previous, err)
		}
		if got != tc.want {
			t.Fatalf("after %s: got %s, want %s", tc.previous, got, tc.want)
		}
		if err := validateTrackedGitLock(LockSource{
			Version:    got,
			Repository: "https://example.invalid/repo.git",
			Ref:        "refs/heads/release",
			Path:       "certdata.txt",
			Commit:     strings.Repeat("a", 40),
			Blob:       strings.Repeat("b", 40),
			BlobSHA256: strings.Repeat("c", 64),
		}, GitSourceConfig{
			URL:         "https://example.invalid/repo.git",
			Ref:         "refs/heads/release",
			TrackedPath: "certdata.txt",
		}, got); err != nil {
			t.Fatalf("generated version %s is not lock-valid: %v", got, err)
		}
	}
}

func TestNextTrackedSnapshotVersionSuffixIsOverflowSafe(t *testing.T) {
	ctx := &Context{Start: time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)}
	history := []LockSource{{Version: "2026.09.06.999999999999999999999999999999"}}
	got, err := nextTrackedSnapshotVersion(ctx, history)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026.09.06.1000000000000000000000000000000" {
		t.Fatalf("next version = %q", got)
	}
}
