package pekit

import (
	"regexp"
	"testing"
)

func TestTrailingZeroCandidates(t *testing.T) {
	got := trailingZeroCandidates("2.43.0")
	want := []string{"2.43.0", "2.43"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestTemplateExtractRegexMatchesFilenameListing(t *testing.T) {
	re, err := templateExtractRegex("foo-{{version}}.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	version := extractVersion(`<a href="foo-1.2.3.tar.gz">foo</a>`, re)
	if version != "1.2.3" {
		t.Fatalf("version = %q", version)
	}
}

func TestGitTagNamedComponentsComposeCanonicalVersion(t *testing.T) {
	tagFilter := regexp.MustCompile(`^(?P<major>[0-9]{4})(?P<minor>[0-9]{2})(?P<patch>[0-9]{2})$`)
	refPattern, err := templateExtractRegex("{{major}}{{minor}}{{patch}}")
	if err != nil {
		t.Fatal(err)
	}
	version, err := extractGitTagVersion("20260810", tagFilter, refPattern)
	if err != nil {
		t.Fatal(err)
	}
	if version != "2026.08.10" {
		t.Fatalf("version = %q, want 2026.08.10", version)
	}
}

func TestGitTagNamedVersionCaptureSuppliesWholeVersion(t *testing.T) {
	tagFilter := regexp.MustCompile(`^release-(?P<version>[0-9]+\.[0-9]+\.[0-9]+)$`)
	refPattern, err := templateExtractRegex("release-{{version}}")
	if err != nil {
		t.Fatal(err)
	}
	version, err := extractGitTagVersion("release-1.2.3", tagFilter, refPattern)
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", version)
	}
}

func TestGitTagUnnamedFilterCaptureDoesNotChangeVersion(t *testing.T) {
	tagFilter := regexp.MustCompile(`^v[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	refPattern, err := templateExtractRegex("v{{version}}")
	if err != nil {
		t.Fatal(err)
	}
	version, err := extractGitTagVersion("v1.47.3", tagFilter, refPattern)
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.47.3" {
		t.Fatalf("version = %q, want 1.47.3", version)
	}
}

func TestValidateConstraintRejectsUnsupportedOperators(t *testing.T) {
	if err := validateConstraintString(">= 1.0, < 2.0"); err != nil {
		t.Fatalf("valid constraint rejected: %v", err)
	}
	if err := validateConstraintString(">=2.32 >=2.43"); err != nil {
		t.Fatalf("space-separated constraint rejected: %v", err)
	}
	if err := validateConstraintString(">= 2.32 < 2.44"); err != nil {
		t.Fatalf("space-separated constraint with spaced operators rejected: %v", err)
	}
	if err := validateConstraintString("^1.0"); err == nil {
		t.Fatal("expected unsupported constraint to fail")
	}
}

func TestFilterVersionsAcceptsSpaceSeparatedConstraints(t *testing.T) {
	available := []string{"2.31", "2.32", "2.43", "2.44"}
	got := filterVersions(available, ">=2.32 <2.44")
	want := []string{"2.32", "2.43"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
