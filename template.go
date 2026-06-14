package main

import (
	"fmt"
	"regexp"
)

// Version is a parsed semantic version supplied via --version, exposed to
// recipes through {{...}} template variables.
type Version struct {
	Full       string // verbatim input, for {{version}}
	Major      string
	Minor      string // "" when the input omits it (a bare-major version like "5")
	Patch      string // "" when the input omits it (a two-component version like "2.43")
	Prerelease string
	Buildmeta  string
}

func (v *Version) lookup(key string) (string, bool) {
	switch key {
	case "version":
		return v.Full, true
	case "major":
		return v.Major, true
	case "minor":
		return v.Minor, true
	case "patch":
		return v.Patch, true
	case "prerelease":
		return v.Prerelease, true
	case "buildmeta":
		return v.Buildmeta, true
	}
	return "", false
}

// localVersion is the default version stamped on a --local build when no
// --version is given: a valid-semver sentinel so every {{...}} variable
// resolves (major/minor/patch = 0, prerelease = localdev), and it sorts
// below every real release so a dev build can never supersede one.
func localVersion() *Version {
	return &Version{Full: "0.0.0-localdev", Major: "0", Minor: "0", Patch: "0", Prerelease: "localdev"}
}

// semverRe parses MAJOR[.MINOR[.PATCH]][-prerelease][+buildmeta]. Minor and
// patch are OPTIONAL so a recipe can pin a two-component release tag (glibc's
// "2.43") or a bare major. An omitted component stays empty rather than
// defaulting to 0: {{version}} then renders exactly what was asked for, and a
// template that needs the missing component errors (see renderTemplate)
// instead of fabricating a zero.
var semverRe = regexp.MustCompile(`^(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)

// parseVersion parses MAJOR[.MINOR[.PATCH]][-prerelease][+buildmeta]. Minor and
// patch are optional (see semverRe); an omitted component is left empty.
func parseVersion(s string) (*Version, error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("invalid --version %q (want MAJOR[.MINOR[.PATCH]][-prerelease][+buildmeta])", s)
	}
	return &Version{Full: s, Major: m[1], Minor: m[2], Patch: m[3], Prerelease: m[4], Buildmeta: m[5]}, nil
}

// templateVar matches a {{name}} placeholder. Double braces dodge shell
// brace expansion ({a,b}, {1..9}), so templates are safe inside build
// commands and single braces are left untouched.
var templateVar = regexp.MustCompile(`\{\{\s*([^}]*?)\s*\}\}`)

// renderTemplate substitutes {{var}} placeholders from v across the whole
// text. An unknown variable is an error (catches typos); any placeholder
// at all when v is nil (no --version given) is an error too.
func renderTemplate(text string, v *Version) (string, error) {
	var rerr error
	out := templateVar.ReplaceAllStringFunc(text, func(match string) string {
		key := templateVar.FindStringSubmatch(match)[1]
		if v == nil {
			rerr = fmt.Errorf("template %q needs --version MAJOR.MINOR.PATCH", match)
			return match
		}
		val, ok := v.lookup(key)
		if !ok {
			rerr = fmt.Errorf("unknown template variable %q", match)
			return match
		}
		// A partial --version (2.43, 5) has no minor/patch; referencing the
		// missing component is an error rather than a silent empty render
		// (which would yield a malformed "2.43." rev). Prerelease/buildmeta
		// legitimately render empty when absent.
		if val == "" && (key == "minor" || key == "patch") {
			rerr = fmt.Errorf("template %q: version %q has no %s component", match, v.Full, key)
			return match
		}
		return val
	})
	if rerr != nil {
		return "", rerr
	}
	return out, nil
}
