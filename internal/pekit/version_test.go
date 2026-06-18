package pekit

import "testing"

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

func TestValidateConstraintRejectsUnsupportedOperators(t *testing.T) {
	if err := validateConstraintString(">= 1.0, < 2.0"); err != nil {
		t.Fatalf("valid constraint rejected: %v", err)
	}
	if err := validateConstraintString("^1.0"); err == nil {
		t.Fatal("expected unsupported constraint to fail")
	}
}
