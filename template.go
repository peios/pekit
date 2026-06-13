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
	Minor      string
	Patch      string
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

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)

// parseVersion parses MAJOR.MINOR.PATCH[-prerelease][+buildmeta].
func parseVersion(s string) (*Version, error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("invalid --version %q (want MAJOR.MINOR.PATCH[-prerelease][+buildmeta])", s)
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
		return val
	})
	if rerr != nil {
		return "", rerr
	}
	return out, nil
}
