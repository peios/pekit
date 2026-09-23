package pekit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const epochProbe = `printf '%s:%s' "$PEKIT_SOURCE_TIMESTAMP" "${SOURCE_DATE_EPOCH-unset}" > "$PEKIT_OUT/epoch"`

func readEpochProbe(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "out", "*", "build", "main", "epoch"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(dir, "out", "build", "main", "epoch"))
	}
	if len(matches) != 1 {
		t.Fatalf("epoch probe outputs = %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func tarGzWithTimes(t *testing.T, root string, times map[string]time.Time) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, mtime := range times {
		if err := tw.WriteHeader(&tar.Header{Name: root + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, ModTime: mtime}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A URL source is dated by its newest archive member, which the locked bytes
// fix, and that date is exported as SOURCE_DATE_EPOCH (PEI-94).
func TestURLSourceExportsSourceDateEpochFromArchive(t *testing.T) {
	dir := t.TempDir()
	newest := time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC)
	serveURLs(t, map[string][]byte{"https://example.test/app-1.0.tar.gz": tarGzWithTimes(t, "app-1.0", map[string]time.Time{
		"old.txt":     newest.Add(-48 * time.Hour),
		"payload.txt": newest,
	})})
	writeFile(t, filepath.Join(dir, "pekit.toml"), `
out_dir = "out"

[source.url]
url = "https://example.test/app-{{version}}.tar.gz"
extract = true
root = "app-{{version}}"

[build]
command = '''`+epochProbe+`'''
`)
	chdir(t, dir)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	want := strconv.FormatInt(newest.Unix(), 10)
	if got := readEpochProbe(t, dir); got != want+":"+want {
		t.Fatalf("PEKIT_SOURCE_TIMESTAMP:SOURCE_DATE_EPOCH = %q, want %s:%s", got, want, want)
	}
}

// A recipe with no upstream source, inside a larger repository, is dated by
// the last commit touching its own directory — not by the repository's HEAD,
// and not by the build (PEI-94).
func TestRecipeSourceDatedByItsOwnLastCommit(t *testing.T) {
	repo := t.TempDir()
	recipe := filepath.Join(repo, "recipes", "app")
	writeFile(t, filepath.Join(recipe, "pekit.toml"), "out_dir = \"out\"\n\n[build]\ncommand = '''"+epochProbe+"'''\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), "out/\n")
	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	at := func(unix int64) []string {
		d := strconv.FormatInt(unix, 10) + " +0000"
		return []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid"}
	}
	git(nil, "init", "-q")
	git(nil, "add", ".")
	git(at(1700000000), "commit", "-q", "-m", "recipe")
	writeFile(t, filepath.Join(repo, "unrelated.txt"), "later\n")
	git(nil, "add", "unrelated.txt")
	git(at(1800000000), "commit", "-q", "-m", "unrelated")

	chdir(t, recipe)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := readEpochProbe(t, recipe); got != "1700000000:1700000000" {
		t.Fatalf("PEKIT_SOURCE_TIMESTAMP:SOURCE_DATE_EPOCH = %q, want the recipe's own commit", got)
	}
}

// A source with no stable date exports no SOURCE_DATE_EPOCH rather than 0,
// which tools would take for 1970.
func TestUndatedSourceExportsNoSourceDateEpoch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), "out_dir = \"out\"\n\n[build]\ncommand = '''"+epochProbe+"'''\n")
	chdir(t, dir)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run([]string{"build"}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := readEpochProbe(t, dir); got != "0:unset" {
		t.Fatalf("PEKIT_SOURCE_TIMESTAMP:SOURCE_DATE_EPOCH = %q, want 0:unset", got)
	}
}
