package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParseFilesExclude(t *testing.T) {
	pf, err := ParsePackageFile(`format = "tar"

[files]
":usr/bin/**" = "usr/bin"
exclude = [":usr/bin/mtrace", ":usr/bin/*trace", "share/doc/**"]
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pf.Exclude) != 3 {
		t.Fatalf("excludes = %v, want 3", pf.Exclude)
	}
	// The ":"-prefixed patterns resolve to target main; the bare one is literal.
	if pf.Exclude[0].Target != "main" || pf.Exclude[0].Path != "usr/bin/mtrace" {
		t.Errorf("exclude[0] = %+v", pf.Exclude[0])
	}
	if pf.Exclude[2].Target != "" || pf.Exclude[2].Path != "share/doc/**" {
		t.Errorf("exclude[2] = %+v, want a literal (target \"\")", pf.Exclude[2])
	}
	// The single glob mapping is still the only [files] entry.
	if len(pf.Files) != 1 || pf.Files[0].Source.Path != "usr/bin/**" {
		t.Errorf("files = %v, want the one glob mapping", pf.Files)
	}
}

func TestParseFilesExcludeRejectsNonString(t *testing.T) {
	if _, err := ParsePackageFile(`format = "tar"

[files]
":usr/bin/**" = "usr/bin"
exclude = [42]
`); err == nil {
		t.Error("a non-string exclude entry should error")
	}
}

// A source literally named "exclude" (string value, not an array) still maps
// normally — the reserved key never shadows a real file.
func TestExcludeKeyAsLiteralFile(t *testing.T) {
	pf, err := ParsePackageFile(`format = "tar"

[files]
exclude = "usr/share/exclude"
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pf.Exclude) != 0 {
		t.Errorf("a string-valued exclude is a mapping, not a filter; got %v", pf.Exclude)
	}
	if len(pf.Files) != 1 || pf.Files[0].Source.Path != "exclude" || pf.Files[0].Dest != "usr/share/exclude" {
		t.Errorf("files = %v, want the literal 'exclude' mapping", pf.Files)
	}
}

func TestExcludedBy(t *testing.T) {
	ex := []SourceRef{
		{Target: "main", Path: "usr/bin/mtrace"},  // exact
		{Target: "main", Path: "usr/bin/*trace"},  // glob family
		{Target: "main", Path: "share/locale/**"}, // subtree
		{Target: "", Path: "README"},              // literal-space
	}
	cases := []struct {
		target, rel string
		want        bool
	}{
		{"main", "usr/bin/mtrace", true},                  // exact
		{"main", "usr/bin/xtrace", true},                  // *trace
		{"main", "usr/bin/ld.so", false},                  // not excluded
		{"main", "share/locale/de/LC_MESSAGES/x", true},   // ** subtree
		{"", "usr/bin/mtrace", false},                     // target mismatch
		{"", "README", true},                              // literal space
		{"main", "README", false},                         // wrong target for literal
	}
	for _, c := range cases {
		got := excludedBy(ex, c.target, c.rel) >= 0
		if got != c.want {
			t.Errorf("excludedBy(%q, %q) = %v, want %v", c.target, c.rel, got, c.want)
		}
	}
}

func TestResolveFilesExcludes(t *testing.T) {
	outDir := t.TempDir()
	stage := filepath.Join(outDir, "build", "main", "usr", "bin")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ld.so", "ldd", "mtrace", "xtrace", "sotruss"} {
		if err := os.WriteFile(filepath.Join(stage, f), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pf, err := ParsePackageFile(`format = "tar"

[files]
":usr/bin/**" = "usr/bin"
exclude = [":usr/bin/mtrace", ":usr/bin/*trace", ":usr/bin/sotruss", ":usr/bin/ghost"]
`)
	if err != nil {
		t.Fatal(err)
	}

	staged, err := resolveFiles(pf, "libc", outDir, outDir)
	if err != nil {
		t.Fatalf("resolveFiles: %v", err)
	}
	var dests []string
	for _, sf := range staged {
		dests = append(dests, sf.Dest)
	}
	sort.Strings(dests)
	// mtrace (exact), xtrace (*trace), sotruss (exact) excluded; ld.so, ldd kept.
	if strings.Join(dests, ",") != "usr/bin/ld.so,usr/bin/ldd" {
		t.Errorf("kept = %v, want [usr/bin/ld.so usr/bin/ldd]", dests)
	}
}
