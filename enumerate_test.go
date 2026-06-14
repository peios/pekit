package main

import (
	"strings"
	"testing"
)

// matchTag is a test helper: invert the template and try one tag.
func matchTag(t *testing.T, tmpl, tag string) (string, bool) {
	t.Helper()
	re, err := revMatcher(tmpl)
	if err != nil {
		t.Fatalf("revMatcher(%q): %v", tmpl, err)
	}
	v, ok := versionFromTag(re, tag)
	if !ok {
		return "", false
	}
	return v.Full, true
}

func TestInvertVersionTemplate(t *testing.T) {
	cases := []struct {
		tag  string
		want string
		ok   bool
	}{
		{"v0.34.0", "0.34.0", true},
		{"v1.2.3-rc1", "1.2.3-rc1", true},
		{"v1.2.3+build5", "1.2.3+build5", true},
		{"v2.43", "2.43", true},   // 2-component (glibc): now captured
		{"v5", "5", true},         // bare major: now captured
		{"v1.2.3.4", "", false},   // 4-component: anchored regex rejects
		{"nightly", "", false},    // no match
		{"prev1.0.0x", "", false}, // anchoring prevents substring match
		{"0.34.0", "", false},     // missing the literal "v"
	}
	for _, c := range cases {
		got, ok := matchTag(t, "v{{version}}", c.tag)
		if ok != c.ok || got != c.want {
			t.Errorf("v{{version}} vs %q = (%q,%v), want (%q,%v)", c.tag, got, ok, c.want, c.ok)
		}
	}
}

func TestInvertComponentTemplate(t *testing.T) {
	// busybox-style underscore tags.
	got, ok := matchTag(t, "{{major}}_{{minor}}_{{patch}}", "1_36_1")
	if !ok || got != "1.36.1" {
		t.Errorf("got (%q,%v), want (1.36.1,true)", got, ok)
	}
	if _, ok := matchTag(t, "{{major}}_{{minor}}_{{patch}}", "1_36"); ok {
		t.Error("1_36 should not match three-component template")
	}
	if _, ok := matchTag(t, "{{major}}_{{minor}}_{{patch}}", "v1_36_1"); ok {
		t.Error("v1_36_1 should not match (no leading v in template)")
	}
}

func TestInvertDottedComponentTemplate(t *testing.T) {
	got, ok := matchTag(t, "v{{major}}.{{minor}}.{{patch}}", "v0.34.0")
	if !ok || got != "0.34.0" {
		t.Errorf("got (%q,%v), want (0.34.0,true)", got, ok)
	}
}

func TestInvertEscapesLiterals(t *testing.T) {
	// The "." and "-" in literal text must be escaped, not regex-special.
	got, ok := matchTag(t, "release-{{version}}", "release-2.0.0")
	if !ok || got != "2.0.0" {
		t.Errorf("got (%q,%v)", got, ok)
	}
	// A "." in the template is literal: "releaseX2.0.0" must NOT match
	// "release-{{version}}" (the '-' is literal).
	if _, ok := matchTag(t, "release-{{version}}", "releaseX2.0.0"); ok {
		t.Error("literal '-' should not match 'X'")
	}
}

func TestVersionLadder(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"2.0.0", []string{"2.0.0", "2.0", "2"}},
		{"2.43.0", []string{"2.43.0", "2.43"}},
		{"2.43", []string{"2.43"}},           // nothing to drop
		{"2.43.9000", []string{"2.43.9000"}}, // non-zero patch: no drop
		{"1.0", []string{"1.0", "1"}},
		{"0.0.0", []string{"0.0.0", "0.0", "0"}}, // major is never dropped
		{"1.2.0-rc1", []string{"1.2.0-rc1"}},     // prerelease pins the triple
	}
	for _, c := range cases {
		v, err := parseVersion(c.in)
		if err != nil {
			t.Fatalf("parseVersion(%q): %v", c.in, err)
		}
		var got []string
		for _, cand := range versionLadder(v) {
			got = append(got, cand.Full)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("versionLadder(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRevMatcherRejectsUnknownVar(t *testing.T) {
	if _, err := revMatcher("v{{ver}}"); err == nil {
		t.Error("want error for unknown {{ver}}")
	}
}

func TestRevMatcherRejectsNoTemplate(t *testing.T) {
	if _, err := revMatcher("v1.2.3"); err == nil {
		t.Error("a literal rev has nothing to enumerate; want error")
	}
}

// versionSet builds a set the way resolveVersions does, for cap tests.
func versionSet(t *testing.T, vs ...string) map[string]*Version {
	t.Helper()
	m := map[string]*Version{}
	for _, s := range vs {
		v, err := parseVersion(s)
		if err != nil {
			t.Fatalf("parseVersion(%q): %v", s, err)
		}
		m[v.Full] = v
	}
	return m
}

func TestCapVersions(t *testing.T) {
	set := versionSet(t, "0.21.0", "0.21.1", "0.21.2")
	excluded, err := capVersions(set, ">=0.21.1")
	if err != nil {
		t.Fatalf("capVersions: %v", err)
	}
	if len(set) != 2 || set["0.21.1"] == nil || set["0.21.2"] == nil {
		t.Errorf("kept = %v, want {0.21.1, 0.21.2}", sortedNames(set))
	}
	if len(excluded) != 1 || excluded[0] != "0.21.0" {
		t.Errorf("excluded = %v, want [0.21.0]", excluded)
	}
}

func TestCapVersionsExcludedIsSorted(t *testing.T) {
	// 0.10.0 must sort before 0.9.0 (semver, not lexical).
	set := versionSet(t, "0.9.0", "0.10.0", "1.0.0")
	excluded, err := capVersions(set, ">=1.0.0")
	if err != nil {
		t.Fatalf("capVersions: %v", err)
	}
	if len(excluded) != 2 || excluded[0] != "0.9.0" || excluded[1] != "0.10.0" {
		t.Errorf("excluded = %v, want [0.9.0 0.10.0]", excluded)
	}
}

func TestCapVersionsBadConstraint(t *testing.T) {
	if _, err := capVersions(versionSet(t, "1.0.0"), "not a constraint"); err == nil {
		t.Error("want error for invalid constraint")
	}
}
