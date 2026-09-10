package pekit

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type Version struct {
	Raw string
	// Parsed reports whether Raw decomposed into the fields below. A
	// Version that did not parse still carries Raw — enough for
	// {{version}} — but every derived token is meaningless, and
	// rendering one is refused rather than substituting an empty string
	// (PEI-422).
	Parsed bool
	// Components preserves every numeric component in the upstream core.
	// Major, Minor, and Patch remain the first three components for recipe
	// compatibility; recipes that need the complete value use Raw/{{version}}.
	Components []string
	Major      string
	Minor      string
	Patch      string
	// Suffix preserves an unseparated alphanumeric suffix used by upstream
	// schemes such as IANA tzdata (2026a, 2026b, ...). It is distinct from a
	// hyphenated prerelease so {{prerelease}} retains its established meaning.
	Suffix     string
	Prerelease string
	BuildMeta  string
}

var versionRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)*)([A-Za-z][0-9A-Za-z]*)?(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)
var embeddedVersionRE = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)*(?:[A-Za-z][0-9A-Za-z]*)?(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?`)

func ParseVersion(raw string) (Version, error) {
	m := versionRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("invalid version %q", raw)
	}
	components := strings.Split(m[1], ".")
	v := Version{
		Raw:        raw,
		Parsed:     true,
		Components: components,
		Suffix:     m[2],
		Prerelease: m[3],
		BuildMeta:  m[4],
	}
	if len(components) > 0 {
		v.Major = components[0]
	}
	if len(components) > 1 {
		v.Minor = components[1]
	}
	if len(components) > 2 {
		v.Patch = components[2]
	}
	return v, nil
}

func (v Version) TemplateVars() map[string]string {
	return map[string]string{
		"version":    v.Raw,
		"major":      v.Major,
		"minor":      v.Minor,
		"patch":      v.Patch,
		"suffix":     v.Suffix,
		"prerelease": v.Prerelease,
		"buildmeta":  v.BuildMeta,
	}
}

func ResolveVersions(ctx *Context, source SourceConfig) ([]Version, error) {
	return resolveVersions(ctx, source, nil)
}

// resolveRecipeVersions is the recipe-aware form used by commands. Ordinary
// sources need only their source table; tracked-path git snapshots also need
// the recipe root because their append-only version history lives in
// pekit.lock.
func resolveRecipeVersions(ctx *Context, recipe RecipeConfig) ([]Version, error) {
	return resolveVersions(ctx, recipe.Source, &recipe)
}

func resolveVersions(ctx *Context, source SourceConfig, recipe *RecipeConfig) ([]Version, error) {
	inv := ctx.Inv
	if willUseLocalSource(inv, source) {
		if inv.Latest || inv.AllVersions || looksLikeConstraint(inv.Version) {
			return nil, diag("unsupported_version_mode", "local sources require an exact --version")
		}
		raw := inv.Version
		if raw == "" {
			raw = "0.0.0-localdev"
		}
		v, err := ParseVersion(raw)
		if err != nil {
			return nil, wrapDiag("invalid_version", "", err)
		}
		return []Version{v}, nil
	}
	if inv.Latest || inv.AllVersions || looksLikeConstraint(inv.Version) {
		versions, err := enumerateSourceVersions(ctx, source, recipe)
		if err != nil {
			if inv.SuppressUnsupportedVersion && !source.HasReproducible() {
				return []Version{{}}, nil
			}
			return nil, err
		}
		versions, err = applyVersionSelector(versions, inv, sourceVersionCap(source))
		if err != nil {
			return nil, err
		}
		out := make([]Version, 0, len(versions))
		for _, raw := range versions {
			v, err := ParseVersion(raw)
			if err != nil {
				return nil, wrapDiag("invalid_version", "", err)
			}
			out = append(out, v)
		}
		return out, nil
	}
	raws := []string{""}
	if inv.Version != "" {
		raws = strings.Split(inv.Version, ",")
		raws = resolveExactVersionTexts(ctx, raws, source, recipe)
	}
	if cap := sourceVersionCap(source); cap != "" && inv.Version != "" {
		if err := validateConstraintString(cap); err != nil {
			return nil, err
		}
		raws = filterVersions(raws, cap)
		if len(raws) == 0 {
			return nil, diag("version_selection_empty", "selected exact versions were filtered out by source version cap")
		}
	}
	out := make([]Version, 0, len(raws))
	for _, raw := range raws {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			out = append(out, Version{})
			continue
		}
		v, err := ParseVersion(raw)
		if err != nil {
			return nil, wrapDiag("invalid_version", "", err)
		}
		out = append(out, v)
	}
	return out, nil
}

func resolveExactVersionTexts(ctx *Context, raws []string, source SourceConfig, recipes ...*RecipeConfig) []string {
	if !source.HasReproducible() {
		return raws
	}
	// A PyPI lock already pins the exact sdist URL and hash. Do not contact the
	// live index merely to apply the Git/templated-URL trailing-zero ladder;
	// this keeps exact locked rebuilds fully replayable when the index is down.
	if source.PyPI.Project != "" || source.Git.TrackedPath != "" {
		return raws
	}
	available, err := enumerateSourceVersions(ctx, source, recipes...)
	if err != nil || len(available) == 0 {
		return raws
	}
	availableSet := stringSet(available)
	out := make([]string, 0, len(raws))
	for _, raw := range raws {
		raw = strings.TrimSpace(raw)
		selected := raw
		for _, candidate := range trailingZeroCandidates(raw) {
			if availableSet[candidate] {
				selected = candidate
				break
			}
		}
		out = append(out, selected)
	}
	return out
}

func trailingZeroCandidates(raw string) []string {
	v, err := ParseVersion(raw)
	if err != nil || v.Suffix != "" || v.Prerelease != "" || v.BuildMeta != "" {
		return []string{raw}
	}
	components := append([]string(nil), v.Components...)
	candidates := []string{raw}
	for len(components) > 1 && components[len(components)-1] == "0" {
		components = components[:len(components)-1]
		candidates = append(candidates, strings.Join(components, "."))
	}
	return candidates
}

func enumerateSourceVersions(ctx *Context, source SourceConfig, recipes ...*RecipeConfig) ([]string, error) {
	var recipe *RecipeConfig
	if len(recipes) > 0 {
		recipe = recipes[0]
	}
	switch {
	case source.Git.URL != "":
		if source.Git.TrackedPath != "" {
			if recipe == nil {
				return nil, diag("tracked_git_recipe", "tracked-path git version discovery requires a loaded recipe")
			}
			return enumerateTrackedGitVersions(ctx, *recipe, source.Git)
		}
		return enumerateGitVersions(source.Git)
	case source.URL.URL != "":
		return enumerateURLVersions(source.URL)
	case source.PyPI.Project != "":
		return enumeratePyPIVersions(ctx, source.PyPI)
	default:
		return nil, diag("version_enumeration_unavailable", "selected source cannot enumerate versions")
	}
}

func enumerateGitVersions(cfg GitSourceConfig) ([]string, error) {
	out, err := commandOutput("", "git", "ls-remote", "--tags", cfg.URL)
	if err != nil {
		return nil, wrapDiag("git_versions", "enumerate git tags", err)
	}
	var tagFilter *regexp.Regexp
	if cfg.TagRegex != "" {
		tagFilter, err = regexp.Compile(cfg.TagRegex)
		if err != nil {
			return nil, wrapDiag("invalid_regex", "source.git.tag_regex", err)
		}
	}
	refPattern, err := templateExtractRegex(cfg.Ref)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/tags/"), "^{}")
		version, err := extractGitTagVersion(ref, tagFilter, refPattern)
		if err != nil {
			return nil, err
		}
		if version != "" {
			seen[version] = true
		}
	}
	return sortedVersions(seen), nil
}

// extractGitTagVersion maps an upstream tag to the version Pekit exposes.
// Named captures make transformations explicit: `version` supplies a complete
// version, while major/minor/patch (plus optional prerelease/buildmeta) compose
// one. Regexes without those names retain the historical ref-template
// extraction behaviour, so a filtering-only capture group cannot accidentally
// change a package's versions.
func extractGitTagVersion(tag string, tagFilter, refPattern *regexp.Regexp) (string, error) {
	if tagFilter != nil {
		match := tagFilter.FindStringSubmatch(tag)
		if match == nil {
			return "", nil
		}
		version, configured, err := versionFromNamedTagCaptures(tag, tagFilter, match)
		if err != nil {
			return "", err
		}
		if configured {
			return version, nil
		}
	}
	return extractVersion(tag, refPattern), nil
}

func versionFromNamedTagCaptures(tag string, re *regexp.Regexp, match []string) (string, bool, error) {
	capture := func(name string) (string, bool) {
		idx := re.SubexpIndex(name)
		if idx < 0 {
			return "", false
		}
		return match[idx], true
	}
	if version, ok := capture("version"); ok {
		if version == "" {
			return "", true, diag("git_versions", "tag %q has an empty named version capture", tag)
		}
		if _, err := ParseVersion(version); err != nil {
			return "", true, wrapDiag("git_versions", "tag "+tag+" named version capture", err)
		}
		return version, true, nil
	}

	major, hasMajor := capture("major")
	minor, hasMinor := capture("minor")
	patch, hasPatch := capture("patch")
	prerelease, hasPrerelease := capture("prerelease")
	buildmeta, hasBuildmeta := capture("buildmeta")
	configured := hasMajor || hasMinor || hasPatch || hasPrerelease || hasBuildmeta
	if !configured {
		return "", false, nil
	}
	if !hasMajor || major == "" {
		return "", true, diag("git_versions", "tag %q uses named version components but has no non-empty major capture", tag)
	}
	if hasPatch && patch != "" && (!hasMinor || minor == "") {
		return "", true, diag("git_versions", "tag %q has a patch capture without a minor capture", tag)
	}

	version := major
	if hasMinor && minor != "" {
		version += "." + minor
	}
	if hasPatch && patch != "" {
		version += "." + patch
	}
	if hasPrerelease && prerelease != "" {
		version += "-" + prerelease
	}
	if hasBuildmeta && buildmeta != "" {
		version += "+" + buildmeta
	}
	if _, err := ParseVersion(version); err != nil {
		return "", true, wrapDiag("git_versions", "tag "+tag+" named version components", err)
	}
	return version, true, nil
}

func enumerateURLVersions(cfg URLSourceConfig) ([]string, error) {
	baseVersions, err := enumerateBaseURLVersions(cfg)
	if err != nil || !cfg.PatchSeries.Configured() {
		return baseVersions, err
	}
	seen := map[string]bool{}
	for _, raw := range baseVersions {
		base, err := ParseVersion(raw)
		if err != nil || base.Minor == "" || base.Suffix != "" || base.Prerelease != "" || base.BuildMeta != "" {
			continue
		}
		// A patch series is based on the upstream major.minor release, not a
		// later roll-up archive that happens to be present in the same listing.
		if base.Patch != "" && base.Patch != "0" {
			continue
		}
		base.Raw = base.Major + "." + base.Minor + ".0"
		base.Patch = "0"
		if !versionBranchCanMatch(base, cfg.Versions) {
			continue
		}
		seen[base.Raw] = true
		levels, err := enumerateURLPatchLevels(cfg.PatchSeries, base)
		if err != nil {
			return nil, err
		}
		for _, level := range levels {
			seen[base.Major+"."+base.Minor+"."+strconv.Itoa(level)] = true
		}
	}
	return sortedVersions(seen), nil
}

func enumerateBaseURLVersions(cfg URLSourceConfig) ([]string, error) {
	listURL := cfg.ListingURL
	if listURL == "" {
		var err error
		listURL, err = urlListingBase(cfg.URL)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pekit/2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, wrapDiag("url_versions", "fetch URL listing", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, diag("url_versions", "fetch URL listing %s: HTTP %s", listURL, resp.Status)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, wrapDiag("url_versions", "read URL listing", err)
	}
	body := string(bodyBytes)
	seen := map[string]bool{}
	if cfg.FileRegex != "" {
		re, err := regexp.Compile(cfg.FileRegex)
		if err != nil {
			return nil, wrapDiag("invalid_regex", "source.url.file_regex", err)
		}
		for _, match := range re.FindAllString(body, -1) {
			if v := embeddedVersionRE.FindString(match); v != "" {
				seen[v] = true
			}
		}
	} else {
		patternTemplate := cfg.URL
		if slash := strings.LastIndex(patternTemplate, "/"); slash >= 0 {
			patternTemplate = patternTemplate[slash+1:]
		}
		pattern, err := templateExtractRegex(patternTemplate)
		if err != nil {
			return nil, err
		}
		for _, match := range pattern.FindAllStringSubmatch(body, -1) {
			if len(match) > 1 && match[1] != "" {
				seen[match[1]] = true
			}
		}
	}
	return sortedVersions(seen), nil
}

// enumerateURLPatchLevels discovers the contiguous numbered patches for one
// major.minor base release. A missing series directory is normal for a newly
// published base release and therefore means no patches; gaps are an error
// because version N cannot be materialised without every patch before it.
func enumerateURLPatchLevels(cfg URLPatchSeriesConfig, base Version) ([]int, error) {
	rendered, err := renderURLPatch(cfg, base, 0)
	if err != nil {
		return nil, err
	}
	listURL, err := urlListingBase(rendered)
	if err != nil {
		return nil, err
	}
	body, status, err := fetchURLListing(listURL)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, nil
		}
		return nil, wrapDiag("url_patch_versions", "fetch patch-series listing", err)
	}
	re, err := urlPatchLevelRegex(cfg, base)
	if err != nil {
		return nil, err
	}
	patchIndex := re.SubexpIndex("patch")
	seen := map[int]bool{}
	for _, match := range re.FindAllStringSubmatch(body, -1) {
		if patchIndex < 0 || patchIndex >= len(match) || match[patchIndex] == "" {
			continue
		}
		level, err := strconv.Atoi(match[patchIndex])
		if err != nil || level <= 0 {
			return nil, diag("url_patch_versions", "patch-series listing %s contains invalid patchlevel %q", listURL, match[patchIndex])
		}
		seen[level] = true
	}
	if len(seen) == 0 {
		return nil, nil
	}
	max := 0
	for level := range seen {
		if level > max {
			max = level
		}
	}
	levels := make([]int, 0, max)
	for level := 1; level <= max; level++ {
		if !seen[level] {
			return nil, diag("url_patch_gap", "patch-series listing %s is missing patchlevel %d before %d", listURL, level, max)
		}
		levels = append(levels, level)
	}
	return levels, nil
}

func urlPatchLevelRegex(cfg URLPatchSeriesConfig, base Version) (*regexp.Regexp, error) {
	if cfg.FileRegex != "" {
		re, err := regexp.Compile(cfg.FileRegex)
		if err != nil {
			return nil, wrapDiag("invalid_regex", "source.url.patch_series.file_regex", err)
		}
		return re, nil
	}
	name := cfg.URL
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	quoted := regexp.QuoteMeta(name)
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{major}}"), regexp.QuoteMeta(base.Major))
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{minor}}"), regexp.QuoteMeta(base.Minor))
	width := "+"
	if cfg.PatchWidth > 0 {
		width = fmt.Sprintf("{%d}", cfg.PatchWidth)
	}
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{patch}}"), `(?P<patch>[0-9]`+width+`)`)
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{version}}"), regexp.QuoteMeta(base.Raw))
	re, err := regexp.Compile(quoted)
	if err != nil {
		return nil, wrapDiag("template_regex", "build patch-series extraction regex", err)
	}
	return re, nil
}

func renderURLPatch(cfg URLPatchSeriesConfig, selected Version, level int) (string, error) {
	patchVersion := urlPatchVersion(cfg, selected, level)
	rendered, err := RenderTemplate(cfg.URL, TemplateContext{Version: patchVersion})
	if err != nil {
		return "", wrapDiag("template", "render source.url.patch_series.url", err)
	}
	return rendered, nil
}

func urlPatchVersion(cfg URLPatchSeriesConfig, selected Version, level int) Version {
	patchVersion := selected
	patchVersion.Patch = strconv.Itoa(level)
	if cfg.PatchWidth > 0 {
		patchVersion.Patch = fmt.Sprintf("%0*d", cfg.PatchWidth, level)
	}
	return patchVersion
}

func fetchURLListing(listURL string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, listURL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "pekit/2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", resp.StatusCode, fmt.Errorf("fetch URL listing %s: HTTP %s", listURL, resp.Status)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(bodyBytes), resp.StatusCode, nil
}

// versionBranchCanMatch avoids probing old patch directories that cannot
// possibly satisfy the source cap. Patchlevels cannot move a version out of
// its major.minor branch, so comparing just those components is conservative.
func versionBranchCanMatch(base Version, constraint string) bool {
	for _, part := range splitConstraintParts(constraint) {
		if part == "" || part == "*" {
			continue
		}
		op := "="
		raw := part
		for _, candidate := range constraintOperators {
			if strings.HasPrefix(part, candidate) {
				op = candidate
				raw = strings.TrimSpace(strings.TrimPrefix(part, candidate))
				break
			}
		}
		rhs, err := ParseVersion(raw)
		if err != nil || rhs.Minor == "" {
			continue
		}
		cmp := compareVersionText(base.Major+"."+base.Minor, rhs.Major+"."+rhs.Minor)
		switch op {
		case ">", ">=":
			if cmp < 0 {
				return false
			}
		case "<", "<=":
			if cmp > 0 {
				return false
			}
		case "=":
			if cmp != 0 {
				return false
			}
		}
	}
	return true
}

func applyVersionSelector(available []string, inv Invocation, cap string) ([]string, error) {
	if cap != "" {
		if err := validateConstraintString(cap); err != nil {
			return nil, err
		}
		available = filterVersions(available, cap)
	}
	if len(available) == 0 {
		return nil, diag("version_selection_empty", "no versions are available after source version filtering")
	}
	var selected []string
	switch {
	case inv.AllVersions:
		selected = available
	case inv.Latest:
		selected = []string{available[len(available)-1]}
	case looksLikeConstraint(inv.Version):
		if err := validateConstraintString(inv.Version); err != nil {
			return nil, err
		}
		selected = filterVersions(available, inv.Version)
	default:
		return nil, diag("invalid_version_selector", "version enumeration requires --latest, --all-versions, or a constraint")
	}
	if len(selected) == 0 {
		return nil, diag("version_selection_empty", "version selector matched no available versions")
	}
	return selected, nil
}

func validateConstraintString(raw string) error {
	for _, part := range splitConstraintParts(raw) {
		if part == "" || part == "*" {
			continue
		}
		for _, op := range constraintOperators {
			if strings.HasPrefix(part, op) {
				v := strings.TrimSpace(strings.TrimPrefix(part, op))
				if _, err := ParseVersion(v); err != nil {
					return diag("invalid_version_constraint", "invalid version constraint %q", part)
				}
				part = ""
				break
			}
		}
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "~") || strings.HasPrefix(part, "^") {
			return diag("unsupported_version_constraint", "unsupported version constraint operator in %q", part)
		}
		if _, err := ParseVersion(part); err != nil {
			return diag("invalid_version_constraint", "invalid version constraint %q", part)
		}
	}
	return nil
}

var constraintOperators = []string{">=", "<=", ">", "<", "="}

func splitConstraintParts(raw string) []string {
	fields := strings.Fields(strings.ReplaceAll(raw, ",", " "))
	parts := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		part := fields[i]
		if isStandaloneConstraintOperator(part) || part == "^" || part == "~" {
			if i+1 < len(fields) {
				parts = append(parts, part+fields[i+1])
				i++
			} else {
				parts = append(parts, part)
			}
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

func isStandaloneConstraintOperator(part string) bool {
	for _, op := range constraintOperators {
		if part == op {
			return true
		}
	}
	return false
}

func sourceVersionCap(source SourceConfig) string {
	if source.Git.Versions != "" {
		return source.Git.Versions
	}
	if source.URL.Versions != "" {
		return source.URL.Versions
	}
	return source.PyPI.Versions
}

func filterVersions(values []string, constraint string) []string {
	parts := splitConstraintParts(constraint)
	out := make([]string, 0, len(values))
	for _, value := range values {
		ok := true
		for _, part := range parts {
			if !matchConstraint(value, part) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, value)
		}
	}
	return out
}

func matchConstraint(value, constraint string) bool {
	if constraint == "" || constraint == "*" {
		return true
	}
	for _, op := range constraintOperators {
		if strings.HasPrefix(constraint, op) {
			rhs := strings.TrimSpace(strings.TrimPrefix(constraint, op))
			cmp := compareVersionText(value, rhs)
			switch op {
			case ">=":
				return cmp >= 0
			case "<=":
				return cmp <= 0
			case ">":
				return cmp > 0
			case "<":
				return cmp < 0
			case "=":
				return cmp == 0
			}
		}
	}
	return compareVersionText(value, constraint) == 0
}

func sortedVersions(seen map[string]bool) []string {
	out := sortedKeys(seen)
	sortStrings(out)
	sortVersionStrings(out)
	return out
}

func sortVersionStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && compareVersionText(values[j-1], values[j]) > 0; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}

func templateExtractRegex(template string) (*regexp.Regexp, error) {
	if !strings.Contains(template, "{{version}}") {
		return embeddedVersionRE, nil
	}
	quoted := regexp.QuoteMeta(template)
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{version}}"), `(`+embeddedVersionRE.String()+`)`)
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{major}}"), `[0-9]+`)
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{minor}}"), `[0-9]+`)
	quoted = strings.ReplaceAll(quoted, regexp.QuoteMeta("{{patch}}"), `[0-9]+`)
	re, err := regexp.Compile(quoted)
	if err != nil {
		return nil, wrapDiag("template_regex", "build version extraction regex", err)
	}
	return re, nil
}

func extractVersion(value string, re *regexp.Regexp) string {
	match := re.FindStringSubmatch(value)
	if len(match) > 1 {
		return match[1]
	}
	return embeddedVersionRE.FindString(value)
}

func urlListingBase(template string) (string, error) {
	idx := strings.Index(template, "{{")
	base := template
	if idx >= 0 {
		base = template[:idx]
	}
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return "", diag("url_versions", "cannot derive listing URL from %q", template)
	}
	return base[:slash+1], nil
}

func willUseLocalSource(inv Invocation, source SourceConfig) bool {
	if inv.Local != nil {
		return true
	}
	if inv.PreferLocal == nil {
		return false
	}
	if ignorePreferLocal(inv) {
		return false
	}
	if *inv.PreferLocal != "" {
		path, err := absPath(inv.Cwd, *inv.PreferLocal)
		return err == nil && dirExists(path)
	}
	return source.Local.ResolvedPath != "" && dirExists(source.Local.ResolvedPath)
}

func ignorePreferLocal(inv Invocation) bool {
	return inv.AllowUnused && inv.PreferLocal != nil && (inv.Latest || inv.AllVersions || looksLikeConstraint(inv.Version))
}

func looksLikeConstraint(raw string) bool {
	return strings.ContainsAny(raw, "<>=~^ ")
}

func compareVersionText(a, b string) int {
	va, ea := ParseVersion(a)
	vb, eb := ParseVersion(b)
	if ea != nil || eb != nil {
		return strings.Compare(a, b)
	}
	componentCount := len(va.Components)
	if len(vb.Components) > componentCount {
		componentCount = len(vb.Components)
	}
	for i := 0; i < componentCount; i++ {
		ac, bc := "0", "0"
		if i < len(va.Components) {
			ac = va.Components[i]
		}
		if i < len(vb.Components) {
			bc = vb.Components[i]
		}
		if cmp := compareNumericComponent(ac, bc); cmp != 0 {
			return cmp
		}
	}
	if cmp := strings.Compare(va.Suffix, vb.Suffix); cmp != 0 {
		return cmp
	}
	return strings.Compare(va.Prerelease, vb.Prerelease)
}

func compareNumericComponent(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
