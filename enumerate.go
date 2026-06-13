package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

// revVarPattern maps each template variable to the regex it expands to
// when we INVERT a rev template into a tag matcher. The classes are
// deliberately specific — a blanket (.+) would over-capture and let
// junk tags through. major/minor/patch are numeric; prerelease/buildmeta
// are semver identifier runs; version is a whole semver. Masterminds is
// still the final arbiter (we re-parse the capture), so the regex only
// needs to extract a faithful candidate, not fully validate it.
var revVarPattern = map[string]string{
	"major":      `\d+`,
	"minor":      `\d+`,
	"patch":      `\d+`,
	"prerelease": `[0-9A-Za-z.-]+`,
	"buildmeta":  `[0-9A-Za-z.-]+`,
	"version":    `\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?`,
}

// revMatcher inverts a rev template (e.g. "v{{version}}",
// "{{major}}_{{minor}}_{{patch}}") into an anchored regex with one named
// capture per variable. Literal text between variables is escaped, so a
// tag must match the template's shape exactly.
func revMatcher(tmpl string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteByte('^')
	vars := 0
	last := 0
	for _, loc := range templateVar.FindAllStringSubmatchIndex(tmpl, -1) {
		b.WriteString(regexp.QuoteMeta(tmpl[last:loc[0]]))
		name := tmpl[loc[2]:loc[3]]
		pat, ok := revVarPattern[name]
		if !ok {
			return nil, fmt.Errorf("rev template: unknown variable {{%s}}", name)
		}
		fmt.Fprintf(&b, `(?P<%s>%s)`, name, pat)
		vars++
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(tmpl[last:]))
	b.WriteByte('$')
	if vars == 0 {
		return nil, fmt.Errorf("rev %q has no {{...}} template, so versions can't be enumerated", tmpl)
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("rev template: %w", err)
	}
	return re, nil
}

// versionFromTag matches one tag against the inverted template and, on a
// match, reconstructs a semver string from the captures and parses it.
// A {{version}} capture is used directly; otherwise major.minor.patch is
// reassembled (with -prerelease / +buildmeta when those vars captured).
// parseVersion is the final gate: a tag that matched the shape but isn't
// real semver (e.g. captured "1.2.3.4" can't, but a stray match could)
// is rejected.
func versionFromTag(re *regexp.Regexp, tag string) (*Version, bool) {
	m := re.FindStringSubmatch(tag)
	if m == nil {
		return nil, false
	}
	caps := map[string]string{}
	for i, name := range re.SubexpNames() {
		if name != "" {
			caps[name] = m[i]
		}
	}
	verStr, ok := caps["version"]
	if !ok {
		verStr = caps["major"] + "." + caps["minor"] + "." + caps["patch"]
		if pr := caps["prerelease"]; pr != "" {
			verStr += "-" + pr
		}
		if bm := caps["buildmeta"]; bm != "" {
			verStr += "+" + bm
		}
	}
	v, err := parseVersion(verStr)
	if err != nil {
		return nil, false
	}
	return v, true
}

// enumerateVersions lists the upstream tags and returns those that match
// the rev template, parsed and de-duplicated. Network call (ls-remote).
func enumerateVersions(git, revTmpl string) ([]*Version, error) {
	re, err := revMatcher(revTmpl)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("git", "ls-remote", "--tags", "--refs", git).Output()
	if err != nil {
		return nil, fmt.Errorf("listing tags of %s: %w", git, err)
	}
	var vers []*Version
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		_, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if v, ok := versionFromTag(re, tag); ok && !seen[v.Full] {
			seen[v.Full] = true
			vers = append(vers, v)
		}
	}
	return vers, nil
}

// resolveVersions turns a --version value into the concrete set to build.
// Comma separates selectors (union). A selector that is a plain version
// is used as-is (no enumeration); one that is a constraint (>3.4.0, ^3,
// 3.x, *) triggers a single upstream enumeration that all constraints
// then filter. Absent --version → one run with no version (nil).
func resolveVersions(raw string, found bool) ([]*Version, error) {
	if !found {
		return []*Version{nil}, nil
	}

	set := map[string]*Version{}
	var constraints []*semver.Constraints
	for _, piece := range strings.Split(raw, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		if v, err := parseVersion(piece); err == nil {
			set[v.Full] = v
			continue
		}
		c, err := semver.NewConstraint(piece)
		if err != nil {
			return nil, fmt.Errorf("--version %q is neither a version nor a constraint", piece)
		}
		constraints = append(constraints, c)
	}

	if len(constraints) > 0 {
		src, err := loadRecipeSource()
		if err != nil {
			return nil, err
		}
		all, err := enumerateVersions(src.Git, src.Rev)
		if err != nil {
			return nil, err
		}
		for _, v := range all {
			sv, err := semver.NewVersion(v.Full)
			if err != nil {
				continue
			}
			for _, c := range constraints {
				if c.Check(sv) {
					set[v.Full] = v
					break
				}
			}
		}
	}

	vers := make([]*Version, 0, len(set))
	for _, v := range set {
		vers = append(vers, v)
	}
	if len(vers) == 0 {
		return nil, fmt.Errorf("--version %q matched no versions", raw)
	}
	sortVersions(vers)
	return vers, nil
}

func sortVersions(vers []*Version) {
	sort.Slice(vers, func(i, j int) bool {
		a, errA := semver.NewVersion(vers[i].Full)
		b, errB := semver.NewVersion(vers[j].Full)
		if errA != nil || errB != nil {
			return vers[i].Full < vers[j].Full
		}
		return a.LessThan(b)
	})
}

// loadRecipeSource reads the recipe's [source] WITHOUT version rendering
// (the raw rev template is what we invert). Constraints can't be resolved
// without a source to enumerate.
func loadRecipeSource() (*Source, error) {
	data, err := os.ReadFile("pekit.toml")
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(string(data))
	if err != nil {
		return nil, fmt.Errorf("pekit.toml: %w", err)
	}
	if cfg.Source == nil {
		return nil, fmt.Errorf("--version constraints need a [source] to enumerate upstream tags")
	}
	return cfg.Source, nil
}
