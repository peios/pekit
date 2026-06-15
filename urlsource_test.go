package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParseSourceURL(t *testing.T) {
	cfg, err := ParseConfig(`outDir = "out"

[source]
url = "https://ftp.gnu.org/gnu/gmp/gmp-{{version}}.tar.xz"
extract = true
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Source.URL != "https://ftp.gnu.org/gnu/gmp/gmp-{{version}}.tar.xz" {
		t.Errorf("url = %q", cfg.Source.URL)
	}
	if !cfg.Source.Extract {
		t.Error("extract not parsed")
	}
}

func TestParseSourceURLErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"url and git", `[source]
git = "https://x/y.git"
rev = "v{{version}}"
url = "https://x/y-{{version}}.tar.xz"`, "mutually exclusive"},
		{"url with rev", `[source]
url = "https://x/y-{{version}}.tar.xz"
rev = "v1"`, "rev"},
		{"extract without url", `[source]
git = "https://x/y.git"
rev = "v1"
extract = true`, "extract"},
		{"none", `[source]
extract = true`, "needs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseConfig("outDir = \"out\"\n\n" + c.src + "\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestParseSourceFileRegex(t *testing.T) {
	// The user's exact recipe shape: url + file_regex + versions together.
	cfg, err := ParseConfig(`outDir = "out"

[source]
url = "https://ftp.gnu.org/gnu/gmp/gmp-{{version}}.tar.xz"
file_regex = '^gmp-\d+\.\d+\.\d+\.tar\.xz$'
versions = ">= 6.3"
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Source.FileRegex != `^gmp-\d+\.\d+\.\d+\.tar\.xz$` {
		t.Errorf("file_regex = %q", cfg.Source.FileRegex)
	}
	if cfg.Source.Versions != ">= 6.3" {
		t.Errorf("versions = %q, want \">= 6.3\"", cfg.Source.Versions)
	}
}

func TestParseSourceRegexSourceMismatch(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"file_regex on git", `[source]
git = "https://x/y.git"
rev = "v{{version}}"
file_regex = '^x$'`, "file_regex"},
		{"tag_regex on url", `[source]
url = "https://x/y-{{version}}.tar.xz"
tag_regex = '^x$'`, "tag_regex"},
		{"bad file_regex", `[source]
url = "https://x/y-{{version}}.tar.xz"
file_regex = '('`, "not a valid regexp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseConfig("outDir = \"out\"\n\n" + c.src + "\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestEnumerateURLVersionsFileRegex(t *testing.T) {
	srv := gmpServer(t, nil)
	// file_regex narrows the .tar.gz matches to just 6.3.0.
	src := &Source{URL: srv.URL + "/gnu/gmp/gmp-{{version}}.tar.gz", FileRegex: `^gmp-6\.3\.0\.tar\.gz$`}
	vers, err := enumerateURLVersions(src)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(vers) != 1 || vers[0].Full != "6.3.0" {
		var got []string
		for _, v := range vers {
			got = append(got, v.Full)
		}
		t.Errorf("with file_regex got %v, want [6.3.0]", got)
	}
}

// TestVersionsCapGE locks the user's ">= 6.3" cap: on the 3-component versions
// a url listing yields, it drops older releases and keeps 6.3.0 and up.
func TestVersionsCapGE(t *testing.T) {
	set := map[string]*Version{}
	for _, s := range []string{"6.2.1", "6.3.0", "6.4.0"} {
		v, err := parseVersion(s)
		if err != nil {
			t.Fatal(err)
		}
		set[s] = v
	}
	excluded, err := capVersions(set, ">= 6.3")
	if err != nil {
		t.Fatalf("capVersions: %v", err)
	}
	if len(set) != 2 || set["6.3.0"] == nil || set["6.4.0"] == nil {
		t.Errorf("kept %d versions, want 6.3.0 and 6.4.0", len(set))
	}
	if strings.Join(excluded, ",") != "6.2.1" {
		t.Errorf("excluded = %v, want [6.2.1]", excluded)
	}
}

func TestSplitURLTemplate(t *testing.T) {
	cases := []struct {
		tmpl, dir, seg string
		err            bool
	}{
		{"https://ftp.gnu.org/gnu/gmp/gmp-{{version}}.tar.xz", "https://ftp.gnu.org/gnu/gmp/", "gmp-{{version}}.tar.xz", false},
		{"https://h/x/v{{version}}/src.tar.gz", "https://h/x/", "v{{version}}", false},
		{"https://h/no-template.tar.gz", "", "", true},
	}
	for _, c := range cases {
		dir, seg, err := splitURLTemplate(c.tmpl)
		if c.err {
			if err == nil {
				t.Errorf("%q: want error", c.tmpl)
			}
			continue
		}
		if err != nil || dir != c.dir || seg != c.seg {
			t.Errorf("split(%q) = (%q,%q,%v), want (%q,%q,nil)", c.tmpl, dir, seg, err, c.dir, c.seg)
		}
	}
}

func TestListingCandidates(t *testing.T) {
	body := `<html><body>
	<a href="../">Parent Directory</a>
	<a href="gmp-6.2.1.tar.xz">gmp-6.2.1.tar.xz</a>
	<a href='gmp-6.3.0.tar.xz?download=1'>gmp-6.3.0.tar.xz</a>
	<a href="/gnu/gmp/gmp-6.3.0.tar.xz.sig">sig</a>
	<a href="subdir/">subdir/</a>
	</body></html>`
	got := listingCandidates(body)
	sort.Strings(got)
	want := "gmp-6.2.1.tar.xz,gmp-6.3.0.tar.xz,gmp-6.3.0.tar.xz.sig,subdir"
	if strings.Join(got, ",") != want {
		t.Errorf("candidates = %v, want %s", got, want)
	}
}

func TestMatchVersionsFilename(t *testing.T) {
	cands := []string{"gmp-6.2.1.tar.xz", "gmp-6.3.0.tar.xz", "gmp-6.3.0.tar.xz.sig", "gmp-6.3.0.tar.gz"}
	vers, err := matchVersions(cands, "gmp-{{version}}.tar.xz", "", "file_regex")
	if err != nil {
		t.Fatalf("matchVersions: %v", err)
	}
	var got []string
	for _, v := range vers {
		got = append(got, v.Full)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "6.2.1,6.3.0" {
		t.Errorf("versions = %v, want [6.2.1 6.3.0] (.sig and .tar.gz rejected)", got)
	}
}

func TestURLScope(t *testing.T) {
	cases := map[string]string{
		"https://ftp.gnu.org/gnu/gmp/gmp-6.3.0.tar.xz": "gmp-6.3.0",
		"https://h/x/foo-1.2.tar.gz":                   "foo-1.2",
		"https://h/x/bar-2.0.zip":                      "bar-2.0",
		"https://h/x/plain-3.bin":                      "plain-3.bin",
	}
	for in, want := range cases {
		if got := urlScope(in); got != want {
			t.Errorf("urlScope(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRoot(t *testing.T) {
	// Single top-level dir → strip into it.
	one := t.TempDir()
	if err := os.Mkdir(filepath.Join(one, "gmp-6.3.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := buildRoot(one); got != filepath.Join(one, "gmp-6.3.0") {
		t.Errorf("buildRoot(single) = %q, want the inner dir", got)
	}
	// Multiple entries → the extraction root itself.
	many := t.TempDir()
	if err := os.Mkdir(filepath.Join(many, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(many, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := buildRoot(many); got != many {
		t.Errorf("buildRoot(many) = %q, want the root", got)
	}
}

// gzTarball builds an in-memory .tar.gz so the fetch/extract path can be
// exercised without xz (which tar would need for a .tar.xz fixture).
func gzTarball(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gmpServer serves a GNU-style listing at /gnu/gmp/ and the 6.3.0 tarball.
func gmpServer(t *testing.T, tarball []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gnu/gmp/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "gmp-6.3.0.tar.gz") {
			w.Write(tarball)
			return
		}
		if r.URL.Path == "/gnu/gmp/" {
			w.Write([]byte(`<html><body>
			<a href="../">Parent Directory</a>
			<a href="gmp-6.2.1.tar.gz">gmp-6.2.1.tar.gz</a>
			<a href="gmp-6.3.0.tar.gz">gmp-6.3.0.tar.gz</a>
			<a href="gmp-6.3.0.tar.gz.sig">gmp-6.3.0.tar.gz.sig</a>
			<a href="gmp-6.3.0.tar.xz">gmp-6.3.0.tar.xz</a>
			</body></html>`))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnumerateURLVersions(t *testing.T) {
	srv := gmpServer(t, nil)
	src := &Source{URL: srv.URL + "/gnu/gmp/gmp-{{version}}.tar.gz"}
	vers, err := enumerateURLVersions(src)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	var got []string
	for _, v := range vers {
		got = append(got, v.Full)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "6.2.1,6.3.0" {
		t.Errorf("versions = %v, want [6.2.1 6.3.0]", got)
	}
}

func TestFetchURLExtracts(t *testing.T) {
	tarball := gzTarball(t, map[string]string{
		"gmp-6.3.0/configure": "#!/bin/sh\n",
		"gmp-6.3.0/README":    "the gnu mp library\n",
	})
	srv := gmpServer(t, tarball)
	src := &Source{URL: srv.URL + "/gnu/gmp/gmp-6.3.0.tar.gz", Extract: true}

	dir := filepath.Join(t.TempDir(), "source")
	root, err := fetchURL(src, dir)
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	// buildRoot stripped into the single gmp-6.3.0/ dir.
	if filepath.Base(root) != "gmp-6.3.0" {
		t.Errorf("build root = %q, want .../gmp-6.3.0", root)
	}
	if data, err := os.ReadFile(filepath.Join(root, "README")); err != nil || string(data) != "the gnu mp library\n" {
		t.Errorf("README = %q, %v", data, err)
	}
	// A second call reuses the cached tree (no re-download/extract).
	root2, err := fetchURL(src, dir)
	if err != nil || root2 != root {
		t.Errorf("cached fetch = (%q,%v), want (%q,nil)", root2, err, root)
	}
}

func TestFetchURLNoExtract(t *testing.T) {
	tarball := gzTarball(t, map[string]string{"x": "y"})
	srv := gmpServer(t, tarball)
	src := &Source{URL: srv.URL + "/gnu/gmp/gmp-6.3.0.tar.gz"} // Extract defaults false

	dir := filepath.Join(t.TempDir(), "source")
	root, err := fetchURL(src, dir)
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	if root != dir {
		t.Errorf("root = %q, want the source dir %q", root, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "gmp-6.3.0.tar.gz")); err != nil {
		t.Errorf("downloaded file not placed in source dir: %v", err)
	}
}

func TestFetchURL404(t *testing.T) {
	srv := gmpServer(t, nil)
	src := &Source{URL: srv.URL + "/gnu/gmp/missing-9.9.9.tar.gz", Extract: true}
	if _, err := fetchURL(src, filepath.Join(t.TempDir(), "source")); err == nil {
		t.Error("a 404 download should error, not extract the error page")
	}
}
