package pekit

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const pypiSimpleBaseURL = "https://pypi.org/simple/"

var (
	pypiProjectNameRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	pypiNameSepRE     = regexp.MustCompile(`[-_.]+`)
	pypiSHA256RE      = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// pypiProjectIndex is cached for one pekit invocation. An initial historical
// sweep therefore performs one metadata request, not one request per version.
// Artifact bytes continue to use the ordinary URL cache and lock machinery.
type pypiProjectIndex struct {
	Project    string
	Candidates map[string][]pypiCandidate
}

type pypiCandidate struct {
	Version  string
	Filename string
	URL      string
	SHA256   string
	Root     string
}

type pypiSimpleProject struct {
	Meta struct {
		APIVersion string `json:"api-version"`
	} `json:"meta"`
	Name  string           `json:"name"`
	Files []pypiSimpleFile `json:"files"`
}

type pypiSimpleFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Hashes   map[string]string `json:"hashes"`
	Yanked   json.RawMessage   `json:"yanked"`
}

func normalizePyPIProject(name string) string {
	return strings.ToLower(pypiNameSepRE.ReplaceAllString(name, "-"))
}

func enumeratePyPIVersions(ctx *Context, cfg PyPISourceConfig) ([]string, error) {
	index, err := loadPyPIProject(ctx, cfg)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(index.Candidates))
	for version := range index.Candidates {
		seen[version] = true
	}
	return sortedVersions(seen), nil
}

func resolvePyPISource(ctx *Context, recipe RecipeConfig, outBase string, cfg PyPISourceConfig, version Version) (SourceState, error) {
	if version.Raw == "" {
		return SourceState{}, diag("missing_version", "source.pypi requires a selected version")
	}
	candidate, locked, err := lockedPyPICandidate(recipe, cfg, version, ctx.Inv.Repin)
	if err != nil {
		return SourceState{}, err
	}
	if !locked {
		index, err := loadPyPIProject(ctx, cfg)
		if err != nil {
			return SourceState{}, err
		}
		candidates := index.Candidates[version.Raw]
		switch len(candidates) {
		case 0:
			return SourceState{}, diag("pypi_sdist_missing",
				"PyPI project %q has no non-yanked, stable, Pekit-compatible sdist for version %s",
				cfg.Project, version.Raw)
		case 1:
			candidate = candidates[0]
		default:
			return SourceState{}, diag("pypi_sdist_ambiguous",
				"PyPI project %q has %d eligible sdists for version %s; refusing to choose between them",
				cfg.Project, len(candidates), version.Raw)
		}
	}

	state, err := resolveURLSource(ctx, recipe, outBase, URLSourceConfig{
		URL:               candidate.URL,
		Extract:           true,
		Root:              candidate.Root,
		Checksum:          "sha256:" + candidate.SHA256,
		ChecksumByVersion: map[string]string{},
	}, version)
	if err != nil {
		return SourceState{}, err
	}
	state.Kind = "pypi"
	state.ProvenanceRef = fmt.Sprintf("pypi:%s@%s#sha256:%s",
		normalizePyPIProject(cfg.Project), version.Raw, candidate.SHA256)
	if !ctx.Inv.DryRun {
		if err := relabelPyPISourceManifest(state); err != nil {
			return SourceState{}, err
		}
	}
	return state, nil
}

func relabelPyPISourceManifest(state SourceState) error {
	manifestPath := filepath.Join(state.WorkBase, "source.pekit.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return wrapDiag("read_file", manifestPath, err)
	}
	var manifest SourceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return wrapDiag("source_manifest", manifestPath, err)
	}
	manifest.Kind = state.Kind
	manifest.ProvenanceRef = state.ProvenanceRef
	return writeSourceManifest(manifestPath, manifest)
}

// lockedPyPICandidate makes a previously selected version independent of the
// live index. The lock already pins both the exact file URL and its digest;
// rebuilding it only needs the ordinary URL artifact cache (or that file URL).
func lockedPyPICandidate(recipe RecipeConfig, cfg PyPISourceConfig, version Version, repin bool) (pypiCandidate, bool, error) {
	if repin {
		return pypiCandidate{}, false, nil
	}
	lock, err := LoadLockFile(recipe.Root)
	if err != nil {
		return pypiCandidate{}, false, err
	}
	entry := lock.Find(version.Raw)
	if entry == nil {
		return pypiCandidate{}, false, nil
	}
	if entry.kind() != "url" {
		return pypiCandidate{}, false, diag("lock_kind_mismatch",
			"version %q is locked as a %s source but the recipe now fetches a PyPI sdist; run `pekit lock --repin --version %s` to accept the change",
			version.Raw, entry.kind(), version.Raw)
	}
	if entry.URL == "" || !pypiSHA256RE.MatchString(entry.SHA256) {
		return pypiCandidate{}, false, diag("invalid_pypi_lock",
			"locked PyPI version %q must contain an exact URL and sha256", version.Raw)
	}
	filename := urlArtifactName(entry.URL)
	lockedVersion, root, ok := parsePyPISDistFilename(cfg.Project, filename)
	if !ok || lockedVersion != version.Raw {
		return pypiCandidate{}, false, diag("invalid_pypi_lock",
			"locked URL for PyPI project %q version %s is not its standardized sdist filename: %s",
			cfg.Project, version.Raw, filename)
	}
	return pypiCandidate{
		Version:  version.Raw,
		Filename: filename,
		URL:      entry.URL,
		SHA256:   strings.ToLower(entry.SHA256),
		Root:     root,
	}, true, nil
}

func loadPyPIProject(ctx *Context, cfg PyPISourceConfig) (pypiProjectIndex, error) {
	project := normalizePyPIProject(cfg.Project)
	if ctx.PyPIProjects != nil {
		if index, ok := ctx.PyPIProjects[project]; ok {
			return index, nil
		}
	}
	endpoint := pypiSimpleBaseURL + url.PathEscape(project) + "/"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return pypiProjectIndex{}, wrapDiag("pypi_index", "create PyPI index request", err)
	}
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	req.Header.Set("User-Agent", "pekit/2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pypiProjectIndex{}, wrapDiag("pypi_index", "fetch PyPI project "+project, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pypiProjectIndex{}, diag("pypi_index", "fetch PyPI project %s: HTTP %s", project, resp.Status)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/vnd.pypi.simple.v1+json" {
		return pypiProjectIndex{}, diag("pypi_index",
			"PyPI project %s returned unsupported Content-Type %q; expected application/vnd.pypi.simple.v1+json",
			project, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil {
		return pypiProjectIndex{}, wrapDiag("pypi_index", "read PyPI project "+project, err)
	}
	if len(body) > 32<<20 {
		return pypiProjectIndex{}, diag("pypi_index", "PyPI project %s response exceeds 32 MiB", project)
	}
	var response pypiSimpleProject
	if err := json.Unmarshal(body, &response); err != nil {
		return pypiProjectIndex{}, wrapDiag("pypi_index", "decode PyPI project "+project, err)
	}
	if err := validatePyPIAPIVersion(response.Meta.APIVersion); err != nil {
		return pypiProjectIndex{}, err
	}
	if normalizePyPIProject(response.Name) != project {
		return pypiProjectIndex{}, diag("pypi_index",
			"PyPI project response names %q, expected %q", response.Name, project)
	}
	base := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL
	}
	index := pypiProjectIndex{Project: project, Candidates: map[string][]pypiCandidate{}}
	for _, file := range response.Files {
		candidate, eligible, err := parsePyPICandidate(project, base, file)
		if err != nil {
			return pypiProjectIndex{}, err
		}
		if !eligible {
			continue
		}
		index.Candidates[candidate.Version] = appendUniquePyPICandidate(index.Candidates[candidate.Version], candidate)
	}
	if ctx.PyPIProjects == nil {
		ctx.PyPIProjects = map[string]pypiProjectIndex{}
	}
	ctx.PyPIProjects[project] = index
	return index, nil
}

func validatePyPIAPIVersion(raw string) error {
	if raw == "" {
		return nil // An omitted version is defined to mean API 1.0.
	}
	majorText, _, _ := strings.Cut(raw, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 1 {
		return diag("pypi_index", "PyPI Simple API returned invalid api-version %q", raw)
	}
	if major > 1 {
		return diag("pypi_index", "PyPI Simple API version %q is newer than Pekit's supported major version 1", raw)
	}
	return nil
}

func parsePyPICandidate(project string, base *url.URL, file pypiSimpleFile) (pypiCandidate, bool, error) {
	version, root, ok := parsePyPISDistFilename(project, file.Filename)
	if !ok {
		return pypiCandidate{}, false, nil
	}
	yanked, err := pypiFileYanked(file.Yanked)
	if err != nil {
		return pypiCandidate{}, false, diag("pypi_index", "file %q has invalid yanked metadata", file.Filename)
	}
	if yanked {
		return pypiCandidate{}, false, nil
	}
	hash := file.Hashes["sha256"]
	if !pypiSHA256RE.MatchString(hash) {
		return pypiCandidate{}, false, diag("pypi_index",
			"eligible PyPI sdist %q has no valid sha256 hash", file.Filename)
	}
	rel, err := url.Parse(file.URL)
	if err != nil {
		return pypiCandidate{}, false, wrapDiag("pypi_index", "parse URL for PyPI sdist "+file.Filename, err)
	}
	resolved := base.ResolveReference(rel)
	if resolved.Scheme != "https" && resolved.Scheme != "http" {
		return pypiCandidate{}, false, diag("pypi_index",
			"eligible PyPI sdist %q resolved to unsupported URL scheme %q", file.Filename, resolved.Scheme)
	}
	return pypiCandidate{
		Version:  version,
		Filename: file.Filename,
		URL:      resolved.String(),
		SHA256:   strings.ToLower(hash),
		Root:     root,
	}, true, nil
}

// parsePyPISDistFilename deliberately recognizes only standardized PEP 625
// sdists. Their single hyphen makes the project/version split unambiguous, and
// their archive root is the filename without .tar.gz. Pekit currently exposes
// only stable versions accepted by its own version grammar; prereleases and
// other PEP 440-only spellings are not automatic update candidates.
func parsePyPISDistFilename(project, filename string) (string, string, bool) {
	if !strings.HasSuffix(filename, ".tar.gz") {
		return "", "", false
	}
	root := strings.TrimSuffix(filename, ".tar.gz")
	if strings.Count(root, "-") != 1 {
		return "", "", false
	}
	name, version, _ := strings.Cut(root, "-")
	if normalizePyPIProject(name) != normalizePyPIProject(project) {
		return "", "", false
	}
	parsed, err := ParseVersion(version)
	if err != nil || parsed.Prerelease != "" {
		return "", "", false
	}
	return version, root, true
}

func pypiFileYanked(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || string(raw) == "false" || string(raw) == "null" {
		return false, nil
	}
	if string(raw) == "true" {
		return true, nil
	}
	var reason string
	if err := json.Unmarshal(raw, &reason); err == nil {
		return true, nil
	}
	return false, fmt.Errorf("expected boolean, string, or null")
}

func appendUniquePyPICandidate(candidates []pypiCandidate, candidate pypiCandidate) []pypiCandidate {
	for _, existing := range candidates {
		if existing.URL == candidate.URL && existing.SHA256 == candidate.SHA256 {
			return candidates
		}
	}
	return append(candidates, candidate)
}
