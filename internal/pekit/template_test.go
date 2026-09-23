package pekit

import (
	"path/filepath"
	"strings"
	"testing"
)

// A package version is peipkg's epoch/upstream/revision grammar, not an
// upstream source version. Read through pekit's source-version regex,
// `1.26.2-3` decomposes with the Peios revision as {{prerelease}}, and
// `2:0.5.0-1` does not decompose at all. A localdir destination is rendered
// from the package version, so it offers {{version}} alone and refuses every
// derived token with a diagnostic (PEI-422).
func TestLocalDirDestinationOffersOnlyThePackageVersion(t *testing.T) {
	recipe := RecipeConfig{Root: t.TempDir()}
	for _, pkgVersion := range []string{"1.26.2-3", "2:0.5.0-1"} {
		inst := PackageInstance{Version: pkgVersion, Artifact: "/stage/pkg.peipkg"}
		for _, token := range []string{"{{major}}", "{{minor}}", "{{patch}}",
			"{{revision}}", "{{revision_suffix}}", "{{suffix}}", "{{prerelease}}", "{{buildmeta}}"} {
			dst, _, err := renderLocalDirDestination(nil, recipe, inst, LocalDirPublish{Path: "dist/" + token})
			if err == nil {
				t.Errorf("%s: path dist/%s = %q, want a diagnostic", pkgVersion, token, dst)
				continue
			}
			if !strings.Contains(err.Error(), "does not decompose") {
				t.Errorf("%s: path dist/%s error = %v, want it to say why", pkgVersion, token, err)
			}
		}
		dst, _, err := renderLocalDirDestination(nil, recipe, inst, LocalDirPublish{Path: "dist/{{version}}"})
		if err != nil {
			t.Fatalf("%s: {{version}}: %v", pkgVersion, err)
		}
		if want := filepath.Join(recipe.Root, "dist", pkgVersion, "pkg.peipkg"); dst != want {
			t.Errorf("%s: {{version}} destination = %q, want %q", pkgVersion, dst, want)
		}
	}
}

// The ordinary case is untouched.
func TestRenderTemplateStillRendersADecomposableVersion(t *testing.T) {
	v := testVersion(t, "1.26.2")
	if !v.Parsed {
		t.Fatal("1.26.2 should parse")
	}
	out, err := RenderTemplate("nginx-{{major}}.{{minor}}/{{version}}", TemplateContext{Version: v})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out != "nginx-1.26/1.26.2" {
		t.Errorf("RenderTemplate = %q", out)
	}
}

func TestRenderTemplatePreservesArbitraryNumericCoreAndFirstThreeCompatibility(t *testing.T) {
	v := testVersion(t, "0.5.13.10")
	out, err := RenderTemplate("dash-{{version}}/{{major}}.{{minor}}.{{patch}}", TemplateContext{Version: v})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out != "dash-0.5.13.10/0.5.13" {
		t.Errorf("RenderTemplate = %q", out)
	}
}

// {{release}} is the rendered package version of the definition being
// rendered, so same-family pins follow a revision bump automatically. It is
// meaningless outside a package manifest and must say so rather than render
// empty.
func TestRenderTemplateRelease(t *testing.T) {
	v := testVersion(t, "1.13.2")
	out, err := RenderTemplate("= {{release}}", TemplateContext{Version: v, Release: "1.13.2-3"})
	if err != nil || out != "= 1.13.2-3" {
		t.Fatalf("RenderTemplate = %q, %v", out, err)
	}
	if _, err := RenderTemplate("= {{release}}", TemplateContext{Version: v}); err == nil ||
		!strings.Contains(err.Error(), "package relations") {
		t.Fatalf("RenderTemplate without a release = %v, want a diagnostic", err)
	}
	meta, err := renderPackageMeta(PackageMeta{
		Name:         "org.example.thing-debuginfo",
		Version:      "{{version}}-3",
		Dependencies: map[string]string{"org.example.thing": "= {{release}}"},
	}, TemplateContext{Version: v})
	if err != nil {
		t.Fatal(err)
	}
	if got := meta.Dependencies["org.example.thing"]; got != "= 1.13.2-3" {
		t.Fatalf("rendered dependency = %q, want = 1.13.2-3", got)
	}
}

func testVersion(t *testing.T, raw string) Version {
	t.Helper()
	v, err := ParseVersion(raw)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", raw, err)
	}
	return v
}

// A peipkg package whose version template renders to something peipkg's
// grammar rejects is refused where the instance is expanded, naming the
// recipe field, instead of surfacing later as a manifest error at pack time
// (PEI-422). tar packages carry no peipkg version and are not checked.
func TestPackageInstanceRejectsAMalformedPeipkgVersion(t *testing.T) {
	v := testVersion(t, "1.26.2")
	source := SourceState{WorkBase: t.TempDir()}
	cfg := func(format, version string) PackageConfig {
		return PackageConfig{Format: format, Package: PackageMeta{
			Name: "org.example.thing", Version: version, Architecture: "x86_64"}}
	}
	inst, err := makePackageInstance("org.example.thing", "", "", cfg("peipkg", "{{version}}-3"), source, v)
	if err != nil || inst.Version != "1.26.2-3" {
		t.Fatalf("well-formed version: %q, %v", inst.Version, err)
	}
	for _, template := range []string{"{{version}}", "{{version}}-", "{{version}}-r3"} {
		_, err := makePackageInstance("org.example.thing", "", "", cfg("peipkg", template), source, v)
		if err == nil || !strings.Contains(err.Error(), "invalid_package_version") ||
			!strings.Contains(err.Error(), "package.version") {
			t.Errorf("version template %q: err = %v, want invalid_package_version naming package.version", template, err)
		}
	}
	if _, err := makePackageInstance("org.example.thing", "", "", cfg("tar", "{{version}}"), source, v); err != nil {
		t.Errorf("tar package: %v", err)
	}
}
