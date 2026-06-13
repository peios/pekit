package main

import "testing"

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
		{"v1.2", "", false},       // 2-component: not semver, skipped
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
