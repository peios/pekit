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
	Parsed     bool
	Major      string
	Minor      string
	Patch      string
	Prerelease string
	BuildMeta  string
}

var versionRE = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)
var embeddedVersionRE = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){0,2}(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?`)

func ParseVersion(raw string) (Version, error) {
	m := versionRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("invalid version %q", raw)
	}
	return Version{Raw: raw, Parsed: true,
		Major: m[1], Minor: m[2], Patch: m[3], Prerelease: m[4], BuildMeta: m[5]}, nil
}

func (v Version) TemplateVars() map[string]string {
	return map[string]string{
		"version":    v.Raw,
		"major":      v.Major,
		"minor":      v.Minor,
		"patch":      v.Patch,
		"prerelease": v.Prerelease,
		"buildmeta":  v.BuildMeta,
	}
}

func ResolveVersions(inv Invocation, source SourceConfig) ([]Version, error) {
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
		versions, err := enumerateSourceVersions(source)
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
		raws = resolveExactVersionTexts(raws, source)
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

func resolveExactVersionTexts(raws []string, source SourceConfig) []string {
	if !source.HasReproducible() {
		return raws
	}
	available, err := enumerateSourceVersions(source)
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
	if err != nil || v.Prerelease != "" || v.BuildMeta != "" {
		return []string{raw}
	}
	candidates := []string{raw}
	if v.Patch == "0" {
		candidates = append(candidates, v.Major+"."+v.Minor)
	}
	if v.Minor == "0" && v.Patch == "0" {
		candidates = append(candidates, v.Major)
	}
	if v.Minor == "0" && v.Patch == "" {
		candidates = append(candidates, v.Major)
	}
	return candidates
}

func enumerateSourceVersions(source SourceConfig) ([]string, error) {
	switch {
	case source.Git.URL != "":
		return enumerateGitVersions(source.Git)
	case source.URL.URL != "":
		return enumerateURLVersions(source.URL)
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
	listURL, err := urlListingBase(cfg.URL)
	if err != nil {
		return nil, err
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
	return source.URL.Versions
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
	for _, pair := range [][2]string{{va.Major, vb.Major}, {va.Minor, vb.Minor}, {va.Patch, vb.Patch}} {
		ai, _ := strconv.Atoi(defaultZero(pair[0]))
		bi, _ := strconv.Atoi(defaultZero(pair[1]))
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return strings.Compare(va.Prerelease, vb.Prerelease)
}

func defaultZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
