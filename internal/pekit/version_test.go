package pekit

import (
	"reflect"
	"regexp"
	"testing"
)

func TestParseVersionPreservesArbitraryNumericCore(t *testing.T) {
	v, err := ParseVersion("0.5.13.5.10-rc1+build.7")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "0.5.13.5.10-rc1+build.7" || v.Major != "0" || v.Minor != "5" || v.Patch != "13" || v.Prerelease != "rc1" || v.BuildMeta != "build.7" {
		t.Fatalf("unexpected parsed version: %#v", v)
	}
	if want := []string{"0", "5", "13", "5", "10"}; !reflect.DeepEqual(v.Components, want) {
		t.Fatalf("components = %v, want %v", v.Components, want)
	}
}

func TestParseVersionPreservesUnseparatedUpstreamSuffix(t *testing.T) {
	v, err := ParseVersion("2026c")
	if err != nil {
		t.Fatal(err)
	}
	if v.Raw != "2026c" || v.Major != "2026" || v.Suffix != "c" || v.Prerelease != "" {
		t.Fatalf("unexpected parsed version: %#v", v)
	}
	if got := v.TemplateVars()["suffix"]; got != "c" {
		t.Fatalf("suffix template value = %q, want c", got)
	}
}

func TestRevisionTemplateVarsRenderRevSuffix(t *testing.T) {
	base, err := ParseVersion("2026.02.10")
	if err != nil {
		t.Fatal(err)
	}
	if got := base.TemplateVars()["revision_suffix"]; got != "" {
		t.Fatalf("base revision suffix = %q", got)
	}

	revision, err := ParseVersion("2026.02.10.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := revision.TemplateVars()["revision"]; got != "1" {
		t.Fatalf("revision = %q", got)
	}
	if got := revision.TemplateVars()["revision_suffix"]; got != "-rev1" {
		t.Fatalf("revision suffix = %q", got)
	}
}

func TestCompareVersionTextUsesAllNumericComponents(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.5.13.9", "0.5.13.10", -1},
		{"1.2.3.4.5", "1.2.3.4.4", 1},
		{"1.2", "1.2.0.0", 0},
		{"1.1000000000000000000000000000001", "1.999999999999999999999999999", 1},
		{"1.0009", "1.9", 0},
	}
	for _, tt := range tests {
		if got := compareVersionText(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersionText(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCompareVersionTextOrdersUnseparatedUpstreamSuffix(t *testing.T) {
	values := []string{"2025c", "2026", "2026a", "2026b", "2026c", "2027a"}
	for i := 1; i < len(values); i++ {
		if got := compareVersionText(values[i-1], values[i]); got >= 0 {
			t.Fatalf("compareVersionText(%q, %q) = %d, want < 0", values[i-1], values[i], got)
		}
	}
	got := filterVersions(values, ">= 2026c")
	want := []string{"2026c", "2027a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered versions = %v, want %v", got, want)
	}
}

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

func TestTrailingZeroCandidatesArbitraryNumericCore(t *testing.T) {
	got := trailingZeroCandidates("2.43.7.0.0")
	want := []string{"2.43.7.0.0", "2.43.7.0", "2.43.7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
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

func TestTemplateExtractRegexMatchesArbitraryNumericCore(t *testing.T) {
	re, err := templateExtractRegex("foo-{{version}}.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	version := extractVersion(`<a href="foo-0.5.13.10.tar.gz">foo</a>`, re)
	if version != "0.5.13.10" {
		t.Fatalf("version = %q", version)
	}
}

func TestTemplateExtractRegexMatchesUnseparatedUpstreamSuffix(t *testing.T) {
	re, err := templateExtractRegex("tzdata{{version}}.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	version := extractVersion(`<a href="tzdata2026c.tar.gz">tzdata</a>`, re)
	if version != "2026c" {
		t.Fatalf("version = %q, want 2026c", version)
	}
}

func TestEmbeddedVersionDoesNotConsumeArchiveExtension(t *testing.T) {
	if got := embeddedVersionRE.FindString("tzdata2026c.tar.gz"); got != "2026c" {
		t.Fatalf("embedded version = %q, want 2026c", got)
	}
}

func TestEnumerateURLVersionsUsesExplicitListingURL(t *testing.T) {
	const listingURL = "https://example.test/releases"
	serveURLs(t, map[string][]byte{
		listingURL: []byte(`<a href="/tag/v3.2.14">stable</a><a href="/tag/v3.2.15-pre1">pre</a>`),
	})

	got, err := enumerateBaseURLVersions(URLSourceConfig{
		URL:        "https://example.test/download/v{{version}}/demo-{{version}}.tar.gz",
		ListingURL: listingURL,
		FileRegex:  `v[0-9]+\.[0-9]+\.[0-9]+[^0-9A-Za-z.+-]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "3.2.14" {
		t.Fatalf("versions = %v, want [3.2.14]", got)
	}
}

func TestEnumerateURLVersionsExtractsAndSortsArbitraryNumericCore(t *testing.T) {
	const listingURL = "https://example.test/releases"
	serveURLs(t, map[string][]byte{
		listingURL: []byte(`<a href="dash-0.5.13.10.tar.gz">new</a><a href="dash-0.5.13.9.tar.gz">old</a>`),
	})

	got, err := enumerateBaseURLVersions(URLSourceConfig{
		URL:        "https://example.test/dash-{{version}}.tar.gz",
		ListingURL: listingURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0.5.13.9", "0.5.13.10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
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

func TestGitTagNamedSuffixAndRevisionComposePostReleaseVersions(t *testing.T) {
	tagFilter := regexp.MustCompile(`^microcode-(?P<major>[0-9]{4})(?P<minor>[0-9]{2})(?P<patch>[0-9]{2})(?:(?P<suffix>[a-z])|-rev(?P<revision>[0-9]+))?$`)
	refPattern, err := templateExtractRegex("microcode-{{major}}{{minor}}{{patch}}{{suffix}}{{revision_suffix}}")
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[string]string{
		"microcode-20260210":      "2026.02.10",
		"microcode-20260210-rev1": "2026.02.10.1",
		"microcode-20230516a":     "2023.05.16a",
	} {
		version, err := extractGitTagVersion(tag, tagFilter, refPattern)
		if err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		if version != want {
			t.Errorf("%s = %q, want %q", tag, version, want)
		}
		parsed, err := ParseVersion(version)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", tag, version, err)
		}
		rendered, err := RenderTemplate("microcode-{{major}}{{minor}}{{patch}}{{suffix}}{{revision_suffix}}", TemplateContext{Version: parsed})
		if err != nil {
			t.Fatalf("%s: render %q: %v", tag, version, err)
		}
		if rendered != tag {
			t.Errorf("render %q = %q, want %q", version, rendered, tag)
		}
	}
}

func TestGitTagNamedVersionCaptureSuppliesWholeVersion(t *testing.T) {
	tagFilter := regexp.MustCompile(`^release-(?P<version>[0-9]+(?:\.[0-9]+)*)$`)
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

func TestGitTagNamedVersionCaptureSuppliesArbitraryNumericCore(t *testing.T) {
	tagFilter := regexp.MustCompile(`^v(?P<version>[0-9]+(?:\.[0-9]+)*)$`)
	refPattern, err := templateExtractRegex("v{{version}}")
	if err != nil {
		t.Fatal(err)
	}
	version, err := extractGitTagVersion("v0.5.13.10", tagFilter, refPattern)
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.5.13.10" {
		t.Fatalf("version = %q, want 0.5.13.10", version)
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

func TestFilterVersionsOrdersArbitraryNumericCore(t *testing.T) {
	available := []string{"0.5.13.8", "0.5.13.9", "0.5.13.10", "0.5.14"}
	got := filterVersions(available, ">=0.5.13.9 <0.5.14")
	want := []string{"0.5.13.9", "0.5.13.10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
