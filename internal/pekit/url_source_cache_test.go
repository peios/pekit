package pekit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Explicitly different commit dates make the former invalidation deterministic.
func commitURLCacheFixture(t *testing.T, dir, date string) {
	t.Helper()
	runTestCmd(t, dir, "git", "add", "-A")
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "fixture")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
}

func TestURLSourceStagesSurviveRecipeDateChange(t *testing.T) {
	for _, checksum := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", checksum), func(t *testing.T) {
			recipe, app, stderr := newPackageCacheRecipe(t, nil)
			payload := []byte("plain download\n")
			serveURLs(t, map[string][]byte{"https://example.test/app-1.0.txt": payload})
			config := strings.ReplaceAll(cacheTestRecipe, ".tar.gz", ".txt")
			config = strings.ReplaceAll(config, "extract = true\nroot = \"app-{{version}}\"", "extract = false")
			if checksum {
				config = strings.Replace(config, "extract = false", fmt.Sprintf("extract = false\nchecksum = \"sha256:%x\"", sha256.Sum256(payload)), 1)
			}
			writeFile(t, filepath.Join(recipe, "pekit.toml"), config)
			for _, member := range []string{"a", "b"} {
				path := filepath.Join(recipe, member+".package.pekit.toml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, strings.ReplaceAll(string(data), "@source:payload.txt", "@source:app-1.0.txt"))
			}
			commitURLCacheFixture(t, recipe, "2026-01-01T00:00:00+00:00")
			if err := app.Run([]string{"package", "--all", "--version", "1.0"}); err != nil {
				t.Fatalf("initial: %v\n%s", err, stderr)
			}
			sources, _ := filepath.Glob(filepath.Join(recipe, "out", "*", "source", "app-1.0.txt"))
			if len(sources) != 1 {
				t.Fatalf("sources: %v", sources)
			}
			before, err := os.Stat(sources[0])
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(recipe, "NOTES"), "recipe notes\n")
			commitURLCacheFixture(t, recipe, "2026-02-01T00:00:00+00:00")
			if err := app.Run([]string{"package", "--all", "--version", "1.0", "--no-build"}); err != nil {
				t.Fatalf("reuse: %v\n%s", err, stderr)
			}
			if got := buildRuns(t, recipe); got != 1 {
				t.Fatalf("build runs = %d", got)
			}
			after, err := os.Stat(sources[0])
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("materialized source was replaced or retimed")
			}
			data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(sources[0])), "source.pekit.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest SourceManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if got := manifest.Timestamp; got != gitWorktreeTimestamp(recipe) {
				t.Fatalf("manifest timestamp = %d, want latest recipe date", got)
			}
			assertNoStaleRefs(t, stagedRecipeRefs(t, recipe), "git:"+gitHead(t, recipe))
		})
	}
}
