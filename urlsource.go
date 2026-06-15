package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// fetchURL materialises a [source] url at dir (the source checkout root) and
// returns the directory the build runs in. The rendered url is downloaded; if
// src.Extract is set the archive is unpacked and the build root is the unpacked
// tree (its single top-level dir, if there is exactly one — the nix sourceRoot
// convention), else the file is downloaded into dir as-is. An existing dir is a
// valid cache (the url is version-specific, so its content is immutable); a
// failed fetch is torn down so the next run retries rather than reusing a
// half-extracted tree.
func fetchURL(src *Source, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err == nil {
		return buildRoot(abs), nil
	}

	base := path.Base(urlPath(src.URL))
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("[source]: url %q has no filename to download", src.URL)
	}
	fmt.Printf("pekit: source: %s\n", src.URL)

	if !src.Extract {
		// No extraction: the downloaded file is the source tree.
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
		if err := httpDownload(src.URL, filepath.Join(abs, base)); err != nil {
			os.RemoveAll(abs)
			return "", err
		}
		return abs, nil
	}

	// Extract: download beside the source dir, unpack into a tmp dir, then
	// rename into place so a cache hit never sees a partial extraction.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	archive := filepath.Join(filepath.Dir(abs), base)
	if err := httpDownload(src.URL, archive); err != nil {
		return "", err
	}
	tmp := abs + ".extracting"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	if err := extractArchive(archive, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return buildRoot(abs), nil
}

// buildRoot is the directory a build runs in after extraction: the single
// top-level subdirectory when an archive unpacks to exactly one (the common
// "name-version/" tarball), else the extraction root itself.
func buildRoot(extractDir string) string {
	entries, err := os.ReadDir(extractDir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return extractDir
	}
	root := filepath.Join(extractDir, entries[0].Name())
	fmt.Printf("pekit: source root: %s\n", root)
	return root
}

// extractArchive unpacks archive into dest, dispatching on extension. tar
// handles its whole compressed family (gz/xz/bz2/zst/uncompressed) by
// autodetection, so the build env just needs the matching decompressor on PATH.
func extractArchive(archive, dest string) error {
	lower := strings.ToLower(archive)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return runExtract("unzip", "-q", archive, "-d", dest)
	case isTarball(lower):
		return runExtract("tar", "xf", archive, "-C", dest)
	default:
		return fmt.Errorf("[source]: don't know how to extract %s (supported: .tar[.gz|.xz|.bz2|.zst], .tgz, .zip)", path.Base(archive))
	}
}

func isTarball(lower string) bool {
	for _, s := range []string{".tar", ".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2", ".tar.zst"} {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

func runExtract(name string, args ...string) error {
	fmt.Printf("pekit: extracting %s\n", filepath.Base(args[len(args)-1]))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extracting with %s: %w", name, err)
	}
	return nil
}

// httpDownload streams url to a file at dest, following redirects. A non-2xx
// status is an error (so a 404 listing page isn't mistaken for an archive).
func httpDownload(url, dest string) error {
	fmt.Printf("pekit: downloading %s\n", url)
	body, err := httpGet(url)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(dest)
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	return f.Close()
}

// httpGetString fetches url and returns its body as a string (for small
// responses like a directory listing).
func httpGetString(url string) (string, error) {
	body, err := httpGet(url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}
	return string(data), nil
}

// httpGet issues a GET and returns the response body on a 2xx, or an error.
// The caller closes the body. A User-Agent is set because some mirrors reject
// blank-UA requests.
func httpGet(rawurl string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, fmt.Errorf("bad url %q: %w", rawurl, err)
	}
	req.Header.Set("User-Agent", "pekit")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", rawurl, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("fetching %s: %s", rawurl, resp.Status)
	}
	return resp.Body, nil
}

// urlScope is the filesystem-safe identifier a url source's stage is keyed by
// (the revScope analogue): the download's filename with any archive extension
// stripped, so "https://.../gmp-6.3.0.tar.xz" scopes to "gmp-6.3.0". It uniquely
// separates one version's build tree from another's under outDir.
func urlScope(rawurl string) string {
	base := path.Base(urlPath(rawurl))
	for _, ext := range []string{".tar.gz", ".tar.xz", ".tar.bz2", ".tar.zst", ".tgz", ".txz", ".tbz2", ".tar", ".zip"} {
		if trimmed, ok := strings.CutSuffix(strings.ToLower(base), ext); ok {
			base = base[:len(trimmed)]
			break
		}
	}
	// Flatten anything that would create stray path segments or odd names.
	base = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, base)
	if base == "" {
		return "download"
	}
	return base
}

// urlPath returns a url's path component, falling back to the raw string if it
// doesn't parse (so a template with {{...}} still yields a usable basename).
func urlPath(rawurl string) string {
	if u, err := url.Parse(rawurl); err == nil && u.Path != "" {
		return u.Path
	}
	return rawurl
}
