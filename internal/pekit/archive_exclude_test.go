package pekit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type exclusionTestEntry struct {
	name, body string
	link       bool
}

func exclusionTestArchive(t *testing.T, format string, entries ...exclusionTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	if format == "tar" {
		w := tar.NewWriter(&buf)
		for _, e := range entries {
			h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}
			if e.link {
				h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.body, 0
			}
			if err := w.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if !e.link {
				if _, err := w.Write([]byte(e.body)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		w := zip.NewWriter(&buf)
		for _, e := range entries {
			h := &zip.FileHeader{Name: e.name, Method: zip.Store}
			h.SetMode(0o644)
			if e.link {
				h.SetMode(os.ModeSymlink | 0o777)
			}
			f, err := w.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestArchiveExclusionsOmitSubtreeBeforeLinkValidation(t *testing.T) {
	for _, format := range []string{"tar", "zip"} {
		for _, root := range []string{".", "qemu-1.0"} {
			t.Run(format+"/"+root, func(t *testing.T) {
				dir := t.TempDir()
				archive := filepath.Join(dir, "source."+format)
				name := func(p string) string { return filepath.ToSlash(filepath.Join(root, p)) }
				data := exclusionTestArchive(t, format,
					exclusionTestEntry{name("roms/edk2/X11"), "/usr/include/X11", true},
					exclusionTestEntry{name("roms/edk2/file"), "omitted", false},
					exclusionTestEntry{name("roms/edk2-other/file"), "kept", false},
					exclusionTestEntry{name("configure"), "configured", false},
					exclusionTestEntry{name("copy"), "configure", true})
				if err := os.WriteFile(archive, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := extractArchive(archive, filepath.Join(dir, "baseline")); err == nil {
					t.Fatal("archive without exclusion must reject escaping symlink")
				}
				out := filepath.Join(dir, "filtered")
				if err := extractArchiveExcluding(archive, out, root, []string{"roms/edk2"}); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(out, name("roms/edk2"))); !os.IsNotExist(err) {
					t.Fatalf("excluded subtree exists: %v", err)
				}
				for p, want := range map[string]string{"roms/edk2-other/file": "kept", "configure": "configured", "copy": "configured"} {
					got, err := os.ReadFile(filepath.Join(out, name(p)))
					if err != nil || string(got) != want {
						t.Fatalf("%s = %q, %v", p, got, err)
					}
				}
			})
		}
	}
}

func TestArchiveExclusionsKeepSafetyChecks(t *testing.T) {
	cases := map[string][]exclusionTestEntry{
		"outside":         {{"../omitted/file", "bad", false}},
		"absolute":        {{"/omitted/file", "bad", false}},
		"other-root":      {{"other/omitted/link", "/outside", true}},
		"prefix-boundary": {{"root/omitted-extra/link", "/outside", true}},
		"retained-link":   {{"root/link", "../../outside", true}},
		"symlink-parent":  {{"root/dir", "other", true}, {"root/dir/file", "bad", false}},
		"collision":       {{"root/file", "first", false}, {"root/file", "second", false}},
	}
	for _, format := range []string{"tar", "zip"} {
		for name, entries := range cases {
			t.Run(format+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				archive := filepath.Join(dir, "source."+format)
				if err := os.WriteFile(archive, exclusionTestArchive(t, format, entries...), 0o600); err != nil {
					t.Fatal(err)
				}
				err := extractArchiveExcluding(archive, filepath.Join(dir, "out"), "root", []string{"omitted"})
				if err == nil || !strings.Contains(err.Error(), "unsafe_archive") {
					t.Fatalf("got %v, want unsafe_archive", err)
				}
			})
		}
	}
}

func TestArchiveExclusionConfiguration(t *testing.T) {
	for _, field := range []string{"source.url", "input.extra"} {
		for _, name := range []string{"", ".", "..", "../escape", "/absolute", "a/../b", "a/", "a//b", "a\\b", "a\x00b", "a/*", "a/??", "a/[b]", "a/{{version}}"} {
			t.Run(field+"/"+fmt.Sprintf("%q", name), func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "pekit.toml")
				writeFile(t, p, fmt.Sprintf("[%s]\nurl = 'https://example.test/source.tar'\nversions = '= 1.0'\nextract = true\nexclude = [%q]\n", field, name))
				if _, err := LoadRecipe(p); err == nil {
					t.Fatal("accepted invalid exclusion")
				}
			})
		}
		p := filepath.Join(t.TempDir(), "pekit.toml")
		writeFile(t, p, fmt.Sprintf("[%s]\nurl = 'https://example.test/source.tar'\nversions = '= 1.0'\nexclude = ['file']\n", field))
		if _, err := LoadRecipe(p); err == nil || !strings.Contains(err.Error(), "requires extract") {
			t.Fatalf("exclude without extract: %v", err)
		}
	}
}

func TestURLExclusionsPatchAndCacheIdentity(t *testing.T) {
	dir := t.TempDir()
	url := "https://example.test/app-1.0.tar"
	data := exclusionTestArchive(t, "tar", exclusionTestEntry{"app-1.0/payload.txt", "payload\n", false}, exclusionTestEntry{"app-1.0/extra", "extra", false})
	responses := map[string][]byte{url: data}
	serveURLs(t, responses)
	writeFile(t, filepath.Join(dir, "patches", "series"), "fix.patch\n")
	writeFile(t, filepath.Join(dir, "patches", "fix.patch"), patchTestDiff)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	for i, excludes := range []string{"", "exclude = ['extra']", ""} {
		recipe := strings.ReplaceAll(patchedURLRecipe, ".tar.gz", ".tar")
		recipe = strings.Replace(recipe, "extract = true", "extract = true\n"+excludes, 1)
		writeFile(t, filepath.Join(dir, "pekit.toml"), recipe)
		if err := app.Run([]string{"build", "--version", "1.0"}); err != nil {
			t.Fatalf("iteration %d: %v\n%s", i, err, &stderr)
		}
		if got := readBuildValue(t, dir); got != "patched\n" {
			t.Fatalf("patch not applied: %q", got)
		}
		// Subsequent materialisations must reuse the authenticated download.
		delete(responses, url)
	}
	sources, _ := filepath.Glob(filepath.Join(dir, "out", "url-*", "source"))
	if len(sources) != 2 {
		t.Fatalf("want distinct filtered/unfiltered source scopes, got %v", sources)
	}
	present := 0
	for _, source := range sources {
		if fileExists(filepath.Join(source, "extra")) {
			present++
		}
	}
	if present != 1 {
		t.Fatalf("extra exists in %d source trees", present)
	}
	artifacts, _ := filepath.Glob(filepath.Join(dir, "out", "_source_cache", "url", "*", "app-1.0.tar"))
	if len(artifacts) != 1 {
		t.Fatalf("download not shared: %v", artifacts)
	}
	got, err := os.ReadFile(artifacts[0])
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("pristine corresponding-source artifact changed")
	}
}

func TestInputExclusionsCacheIdentity(t *testing.T) {
	dir := t.TempDir()
	responses := map[string][]byte{"https://example.test/linux-7.0.9.tar": exclusionTestArchive(t, "tar", exclusionTestEntry{"linux-7.0.9/payload.txt", "input", false}, exclusionTestEntry{"linux-7.0.9/extra", "extra", false})}
	serveURLs(t, responses)
	chdir(t, dir)
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	for _, excludes := range []string{"", "exclude = ['extra']", ""} {
		recipe := `[input.kernel]
url = 'https://example.test/linux-7.0.9.tar'
versions = '= 7.0.9'
extract = true
root = 'linux-7.0.9'
` + excludes + `
[build]
command = '''
set -eu
cat "$PEKIT_INPUT_KERNEL/payload.txt" > "$PEKIT_OUT/input"
if test -e "$PEKIT_INPUT_KERNEL/extra"; then echo present; else echo absent; fi > "$PEKIT_RECIPE_ROOT/result.txt"
'''
`
		writeFile(t, filepath.Join(dir, "pekit.toml"), recipe)
		if err := app.Run([]string{"build"}); err != nil {
			t.Fatalf("%v\n%s", err, &stderr)
		}
		want := "present\n"
		if excludes != "" {
			want = "absent\n"
		}
		if got := readBuildValue(t, dir); got != want {
			t.Fatalf("input exclusion %q gave %q, want %q", excludes, got, want)
		}
		delete(responses, "https://example.test/linux-7.0.9.tar")
	}
}
