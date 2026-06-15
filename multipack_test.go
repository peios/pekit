package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestParseMultipack(t *testing.T) {
	raw := map[string]any{
		"multipack": map[string]any{
			"enum":   []any{int64(1), int64(2), "en"},
			"suffix": "-{{multipack}}",
		},
	}
	mp, err := parseMultipack(raw)
	if err != nil {
		t.Fatalf("parseMultipack: %v", err)
	}
	if strings.Join(mp.Values, ",") != "1,2,en" {
		t.Errorf("values = %v, want [1 2 en] (ints stringified, order kept)", mp.Values)
	}
	if mp.Suffix != "-{{multipack}}" {
		t.Errorf("suffix = %q, want the deferred template", mp.Suffix)
	}
}

func TestParseMultipackAbsent(t *testing.T) {
	mp, err := parseMultipack(map[string]any{"format": "tar"})
	if err != nil {
		t.Fatalf("parseMultipack: %v", err)
	}
	if mp != nil {
		t.Errorf("no [multipack] section should yield nil, got %+v", mp)
	}
}

func TestParseMultipackErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"not a table", map[string]any{"multipack": "x"}, "must be a table"},
		{"missing enum", map[string]any{"multipack": map[string]any{"suffix": "-x"}}, "enum"},
		{"empty enum", map[string]any{"multipack": map[string]any{"enum": []any{}}}, "non-empty"},
		{"bad enum type", map[string]any{"multipack": map[string]any{"enum": []any{1.5}}}, "strings or integers"},
		{"empty enum value", map[string]any{"multipack": map[string]any{"enum": []any{""}}}, "non-empty"},
		{"duplicate enum", map[string]any{"multipack": map[string]any{"enum": []any{"a", "a"}}}, "duplicate"},
		{"unknown key", map[string]any{"multipack": map[string]any{"enum": []any{"a"}, "nope": 1}}, "unknown key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseMultipack(c.raw)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestSubstituteMultipack(t *testing.T) {
	cases := map[string]string{
		"locale/{{multipack}}/x":   "locale/de/x",
		"{{ multipack }}":          "de",          // whitespace tolerated
		"v{{version}}-{{multipack}}": "v{{version}}-de", // other vars left intact
		"plain":                    "plain",
	}
	for in, want := range cases {
		if got := substituteMultipack(in, "de"); got != want {
			t.Errorf("substituteMultipack(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderMultipackValue: a deep render substitutes in map keys (where
// [files] sources live), values, and nested arrays — and leaves the input
// untouched so the same base expands cleanly for the next value.
func TestRenderMultipackValue(t *testing.T) {
	base := map[string]any{
		"files": map[string]any{
			":locale/{{multipack}}/locale.conf": "etc/locale.conf",
		},
		"package": map[string]any{
			"description": "the {{multipack}} locale",
		},
		"dependencies": []any{"libc-{{multipack}}"},
	}
	out, err := renderMultipackValue(base, "de")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := out.(map[string]any)
	files := m["files"].(map[string]any)
	if _, ok := files[":locale/de/locale.conf"]; !ok {
		t.Errorf("key not rendered: %v", files)
	}
	if d := m["package"].(map[string]any)["description"]; d != "the de locale" {
		t.Errorf("value not rendered: %q", d)
	}
	if dep := m["dependencies"].([]any)[0]; dep != "libc-de" {
		t.Errorf("array element not rendered: %q", dep)
	}
	// Input untouched.
	if _, ok := base["files"].(map[string]any)[":locale/{{multipack}}/locale.conf"]; !ok {
		t.Error("renderMultipackValue mutated its input")
	}
}

func TestContainsMultipackVar(t *testing.T) {
	yes := map[string]any{"files": map[string]any{":x/{{multipack}}/y": "z"}}
	no := map[string]any{"files": map[string]any{":x/{{version}}/y": "z"}}
	if !containsMultipackVar(yes) {
		t.Error("should detect {{multipack}} in a key")
	}
	if containsMultipackVar(no) {
		t.Error("{{version}} is not {{multipack}}")
	}
}

// TestRenderTemplateDefersMultipack: the version pass leaves {{multipack}}
// verbatim (and tolerates it even with no --version), while still resolving
// and validating every other variable.
func TestRenderTemplateDefersMultipack(t *testing.T) {
	v, err := parseVersion("2.43")
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderTemplate(`v{{version}}/{{multipack}}`, v, "multipack")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "v2.43/{{multipack}}" {
		t.Errorf("got %q, want v2.43/{{multipack}}", got)
	}
	// Deferred even without a version.
	if got, err := renderTemplate(`{{multipack}}`, nil, "multipack"); err != nil || got != "{{multipack}}" {
		t.Errorf("nil-version defer: got %q err %v", got, err)
	}
	// Not deferred when not listed: {{multipack}} stays an unknown variable
	// (this is why pekit.toml rejects it).
	if _, err := renderTemplate(`{{multipack}}`, v); err == nil || !strings.Contains(err.Error(), "unknown template variable") {
		t.Errorf("without deferral err = %v, want unknown-variable", err)
	}
}

// TestMultipackFanOut drives buildPackages end to end: one package.pekit.toml
// with [multipack] becomes one package per enum value, names get the suffix,
// each picks its own {{multipack}} slice from a build that runs exactly once.
func TestMultipackFanOut(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `outDir = "out"

[build]
command = """
mkdir -p "$PEKIT_OUT/locale/en" "$PEKIT_OUT/locale/de"
echo english > "$PEKIT_OUT/locale/en/locale.conf"
echo deutsch > "$PEKIT_OUT/locale/de/locale.conf"
echo "ran" >> "$PEKIT_OUT/../../../buildcount"
"""
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `format = "tar"

[multipack]
enum = ["en", "de"]
suffix = "-{{multipack}}"

[package]
name = "locale"

[files]
":locale/{{multipack}}/locale.conf" = "etc/locale.conf"
`)
	t.Chdir(dir)

	results, _, err := buildPackages("", nil, false, noBuildSet{})
	if err != nil {
		t.Fatalf("buildPackages: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d packages, want 2", len(results))
	}
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "locale-de,locale-en" {
		t.Errorf("names = %v, want [locale-de locale-en]", names)
	}

	// The shared build ran exactly once for both packages.
	count, err := os.ReadFile(filepath.Join(dir, "buildcount"))
	if err != nil {
		t.Fatalf("buildcount: %v", err)
	}
	if got := strings.Count(string(count), "ran"); got != 1 {
		t.Errorf("build ran %d times, want 1 (shared across multipack)", got)
	}

	// Each artifact carries its own locale's file.
	for _, r := range results {
		_, contents := readTar(t, r.Artifact)
		want := "english\n"
		if r.Name == "locale-de" {
			want = "deutsch\n"
		}
		if contents["etc/locale.conf"] != want {
			t.Errorf("%s: etc/locale.conf = %q, want %q", r.Name, contents["etc/locale.conf"], want)
		}
	}
}

// TestMultipackUsedWithoutSection: a recipe that references {{multipack}} but
// declares no [multipack] is rejected, not shipped with a literal placeholder.
func TestMultipackUsedWithoutSection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `outDir = "out"

[build]
command = "true"
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `format = "tar"

[files]
":locale/{{multipack}}/locale.conf" = "etc/locale.conf"
`)
	t.Chdir(dir)

	_, _, err := buildPackages("", nil, false, noBuildSet{})
	if err == nil || !strings.Contains(err.Error(), "no [multipack] section") {
		t.Fatalf("err = %v, want a 'no [multipack] section' error", err)
	}
}

// TestMultipackNameCollision: an enum whose suffix does not vary with
// {{multipack}} collapses every value onto one name, which is rejected up
// front (before any packing).
func TestMultipackNameCollision(t *testing.T) {
	bc := &buildContext{cfg: &Config{}}
	merged := map[string]any{
		"format": "tar",
		"package": map[string]any{
			"name": "locale",
		},
		"files": map[string]any{
			":x/{{multipack}}": "etc/x",
		},
		"multipack": map[string]any{
			"enum":   []any{"en", "de"},
			"suffix": "-static", // constant: both values -> "locale-static"
		},
	}
	mp, err := parseMultipack(merged)
	if err != nil {
		t.Fatal(err)
	}
	_, err = expandMultipack(bc, merged, mp, nil, "")
	if err == nil || !strings.Contains(err.Error(), "both produce package name") {
		t.Fatalf("err = %v, want a name-collision error", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseMultipackFiles(t *testing.T) {
	raw := map[string]any{
		"multipack": map[string]any{
			"enum": map[string]any{
				"files": map[string]any{
					"path":  "locale:usr/lib/locale/*",
					"regex": `^([a-z]+)_`,
				},
			},
			"suffix": "-{{multipack}}",
		},
	}
	mp, err := parseMultipack(raw)
	if err != nil {
		t.Fatalf("parseMultipack: %v", err)
	}
	if mp.Values != nil {
		t.Errorf("derived enum should leave Values nil, got %v", mp.Values)
	}
	if mp.Files == nil {
		t.Fatal("Files not set for enum.files form")
	}
	if mp.Files.Source.Target != "locale" || mp.Files.Source.Path != "usr/lib/locale/*" {
		t.Errorf("source = %+v, want target=locale path=usr/lib/locale/*", mp.Files.Source)
	}
	if mp.Files.Regex.String() != `^([a-z]+)_` {
		t.Errorf("regex = %q", mp.Files.Regex.String())
	}
}

func TestParseMultipackFilesErrors(t *testing.T) {
	mk := func(files map[string]any) map[string]any {
		return map[string]any{"multipack": map[string]any{"enum": map[string]any{"files": files}}}
	}
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"missing path", mk(map[string]any{"regex": "x"}), "missing required key \"path\""},
		{"missing regex", mk(map[string]any{"path": "a:b"}), "missing required key \"regex\""},
		{"bad regex", mk(map[string]any{"path": "a:b", "regex": "("}), "not a valid regexp"},
		{"no capture group", mk(map[string]any{"path": "a:b", "regex": "x"}), "capture group"},
		{"unknown files key", mk(map[string]any{"path": "a:b", "regex": "(x)", "nope": 1}), "enum.files: unknown key"},
		{
			"enum table not files",
			map[string]any{"multipack": map[string]any{"enum": map[string]any{"dirs": map[string]any{}}}},
			"unknown enum key",
		},
		{
			"enum bad type",
			map[string]any{"multipack": map[string]any{"enum": 42}},
			"array of values or an enum.files table",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseMultipack(c.raw)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

// TestEnumerateMultipackValues globs a locale-like tree and captures language
// codes: many entries collapse to one code, non-matching entries are skipped,
// and the result is distinct and sorted.
func TestEnumerateMultipackValues(t *testing.T) {
	root := t.TempDir()
	loc := filepath.Join(root, "usr", "lib", "locale")
	if err := os.MkdirAll(loc, 0o755); err != nil {
		t.Fatal(err)
	}
	// en_US, en_GB → en; de_DE → de; aa_DJ → aa; C, POSIX, locale.alias skipped.
	for _, name := range []string{"en_US", "en_GB", "de_DE", "aa_DJ", "C", "POSIX", "locale.alias"} {
		if err := os.Mkdir(filepath.Join(loc, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := SourceRef{Target: "locale", Path: "usr/lib/locale/*"}
	re := regexp.MustCompile(`^([a-z]+)_`)
	got, err := enumerateMultipackValues(root, src, re)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if strings.Join(got, ",") != "aa,de,en" {
		t.Errorf("values = %v, want [aa de en] (distinct, sorted, non-matches skipped)", got)
	}
}

func TestEnumerateMultipackValuesNoMatch(t *testing.T) {
	root := t.TempDir()
	loc := filepath.Join(root, "loc")
	if err := os.MkdirAll(loc, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"C", "POSIX"} { // none match ^([a-z]+)_
		if err := os.Mkdir(filepath.Join(loc, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := SourceRef{Target: "x", Path: "loc/*"}
	_, err := enumerateMultipackValues(root, src, regexp.MustCompile(`^([a-z]+)_`))
	if err == nil || !strings.Contains(err.Error(), "matched none") {
		t.Fatalf("err = %v, want a 'matched none' error", err)
	}
}

// TestMultipackFanOutFromFiles drives buildPackages end to end with a derived
// enum: the build stages locale-like entries, enum.files captures the distinct
// language codes, and one package per code is built — each shipping its own
// locales — off a single build run.
func TestMultipackFanOutFromFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pekit.toml"), `outDir = "out"

[build.locale]
command = """
mkdir -p "$PEKIT_OUT/usr/share/loc"
cd "$PEKIT_OUT/usr/share/loc"
for l in aa_DJ en_US en_GB de_DE C; do echo "$l" > "$l"; done
echo ran >> "$PEKIT_OUT/../../../buildcount"
"""
`)
	writeFile(t, filepath.Join(dir, "package.pekit.toml"), `format = "tar"

[multipack]
enum.files.path = "locale:usr/share/loc/*"
enum.files.regex = '^([a-z]+)_'
suffix = "-{{multipack}}"

[package]
name = "locale"

[files]
"locale:usr/share/loc/{{multipack}}_*" = "usr/share/loc"
`)
	t.Chdir(dir)

	results, _, err := buildPackages("", nil, false, noBuildSet{})
	if err != nil {
		t.Fatalf("buildPackages: %v", err)
	}
	var names []string
	for _, r := range results {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "locale-aa,locale-de,locale-en" {
		t.Fatalf("names = %v, want [locale-aa locale-de locale-en]", names)
	}

	// The build (referenced by enum AND by every package's [files]) ran once.
	count, err := os.ReadFile(filepath.Join(dir, "buildcount"))
	if err != nil {
		t.Fatalf("buildcount: %v", err)
	}
	if got := strings.Count(string(count), "ran"); got != 1 {
		t.Errorf("build ran %d times, want 1", got)
	}

	// locale-en pulls both en_US and en_GB; locale-de just de_DE.
	for _, r := range results {
		_, contents := readTar(t, r.Artifact)
		switch r.Name {
		case "locale-en":
			if _, ok := contents["usr/share/loc/en_US"]; !ok {
				t.Errorf("locale-en missing en_US: %v", keysOf(contents))
			}
			if _, ok := contents["usr/share/loc/en_GB"]; !ok {
				t.Errorf("locale-en missing en_GB: %v", keysOf(contents))
			}
			if _, ok := contents["usr/share/loc/de_DE"]; ok {
				t.Errorf("locale-en should not carry de_DE")
			}
		case "locale-de":
			if _, ok := contents["usr/share/loc/de_DE"]; !ok {
				t.Errorf("locale-de missing de_DE: %v", keysOf(contents))
			}
		}
	}
}

func keysOf(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
